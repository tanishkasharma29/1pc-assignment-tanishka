// Package cachemw implements the generic HTTP response caching middleware.
//
// It knows nothing about posts, todos, or any other resource: it operates purely
// on http.Request and http.ResponseWriter, so adding a new cached route is a
// route registration and a policy entry, with no change here. Its own tests
// exercise it against a stub handler for exactly this reason.
//
// # The guarantee
//
// For a given key, at most one request performs the underlying fetch. Every
// other request — whether it arrives an hour later or in the same microsecond —
// is answered from the cache. Two mechanisms are needed, because a cache alone
// does not provide this:
//
//	singleflight  closes the concurrent case. Without it, N requests that arrive
//	              before the first fill completes all miss and all fetch.
//	write-ordering closes the sequential case. The entry is stored, and the store
//	              acknowledged, before any byte of the response is flushed, so a
//	              client cannot receive an answer and re-ask faster than the
//	              cache can be populated.
package cachemw

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/lock"
)

// Cache status values reported in the X-Cache response header.
const (
	StatusHit = "HIT"
	// StatusMiss marks the one request per key that actually performed the fetch.
	StatusMiss = "MISS"
	// StatusCoalesced marks a waiter that was released by the leader but could
	// not read the entry back, so it was served the leader's copy directly. It
	// should be rare; a sustained rate of it means cache writes are failing.
	StatusCoalesced = "HIT-COALESCED"
	// StatusBypass marks a route with caching switched off.
	StatusBypass = "BYPASS"
)

// Options configures the middleware itself, as opposed to any individual route.
type Options struct {
	Enabled      bool
	MaxBodyBytes int64
	StoreHeaders []string
	DebugHeaders bool
	// FetchTimeout bounds a leader's fetch. It is applied to a context detached
	// from the originating request, so it is the only thing stopping a fetch
	// whose client has gone away.
	FetchTimeout time.Duration
	// LockWait controls how long an instance waits for a peer's fill before
	// giving up and fetching itself.
	LockWaitTimeout  time.Duration
	LockPollInterval time.Duration
}

// Middleware caches HTTP responses.
type Middleware struct {
	cache  cache.Cache
	locker lock.Locker
	log    *slog.Logger
	opts   Options

	// group admits one in-flight fetch per key within this process.
	group singleflight.Group

	hits         atomic.Uint64
	misses       atomic.Uint64
	coalesced    atomic.Uint64
	stored       atomic.Uint64
	bypassed     atomic.Uint64
	peerFills    atomic.Uint64
	peerTimeouts atomic.Uint64
	storeErrors  atomic.Uint64
}

func New(c cache.Cache, locker lock.Locker, opts Options, log *slog.Logger) *Middleware {
	if locker == nil {
		locker = lock.NoOp{}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 10 * time.Second
	}
	if opts.LockPollInterval <= 0 {
		opts.LockPollInterval = 25 * time.Millisecond
	}
	return &Middleware{cache: c, locker: locker, log: log, opts: opts}
}

// Stats reports counters for logging and the readiness endpoint. Hits versus
// misses is the headline number: it is a direct measure of calls not paid for.
type Stats struct {
	Hits         uint64      `json:"hits"`
	Misses       uint64      `json:"misses"`
	Coalesced    uint64      `json:"coalesced"`
	Stored       uint64      `json:"stored"`
	Bypassed     uint64      `json:"bypassed"`
	PeerFills    uint64      `json:"peer_fills"`
	PeerTimeouts uint64      `json:"peer_timeouts"`
	StoreErrors  uint64      `json:"store_errors"`
	HitRatio     float64     `json:"hit_ratio"`
	Backend      cache.Stats `json:"backend"`
}

func (m *Middleware) Stats() Stats {
	hits, misses := m.hits.Load(), m.misses.Load()
	var ratio float64
	if total := hits + misses; total > 0 {
		ratio = float64(hits) / float64(total)
	}
	return Stats{
		Hits: hits, Misses: misses,
		Coalesced:    m.coalesced.Load(),
		Stored:       m.stored.Load(),
		Bypassed:     m.bypassed.Load(),
		PeerFills:    m.peerFills.Load(),
		PeerTimeouts: m.peerTimeouts.Load(),
		StoreErrors:  m.storeErrors.Load(),
		HitRatio:     ratio,
		Backend:      m.cache.Stats(),
	}
}

// For returns a middleware bound to a specific route policy. This is the
// registration point: mounting it is how a route opts into caching, and the
// policy is how that caching is tuned.
func (m *Middleware) For(policy Policy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.serve(w, r, next, policy)
		})
	}
}

func (m *Middleware) serve(w http.ResponseWriter, r *http.Request, next http.Handler, policy Policy) {
	// Only GET is cacheable here. Other methods may have side effects, and
	// replaying a stored response for them would be incorrect.
	if !m.opts.Enabled || !policy.Enabled || r.Method != http.MethodGet {
		m.bypassed.Add(1)
		w.Header().Set("X-Cache", StatusBypass)
		next.ServeHTTP(w, r)
		return
	}

	key := buildKey(r, policy)
	ctx := r.Context()

	// Fast path: already cached, no coordination needed at all.
	if entry, ok, err := m.cache.Get(ctx, key.hash); err == nil && ok {
		m.hits.Add(1)
		m.writeEntry(w, r, entry, StatusHit, key)
		return
	}

	// Slow path. singleflight elects one leader per key; everyone else waits.
	//
	// `leader` is captured per call. singleflight runs the closure only in the
	// leader's goroutine, so a follower's copy stays false. The `shared` return
	// value cannot be used for this: it is true for the leader as well whenever
	// anyone waited on it.
	var leader bool
	result, _, _ := m.group.Do(key.hash, func() (any, error) {
		leader = true
		return m.fill(r, next, policy, key), nil
	})
	entry, _ := result.(*cache.Entry)
	if entry == nil {
		// Defensive: fill always returns an entry, so reaching here means a bug
		// rather than a cache miss. Fail loudly rather than serve nothing.
		m.log.ErrorContext(ctx, "cache fill produced no entry", slog.String("key", key.readable))
		http.Error(w, `{"error":{"code":"internal_error","message":"internal server error"}}`,
			http.StatusInternalServerError)
		return
	}

	if leader {
		m.misses.Add(1)
		m.writeEntry(w, r, entry, StatusMiss, key)
		return
	}

	// Followers re-read the cache rather than using the leader's value directly.
	// This is what makes "served from the cache" literally true for every
	// request after the first, instead of merely true in effect.
	if cached, ok, err := m.cache.Get(ctx, key.hash); err == nil && ok {
		m.hits.Add(1)
		m.writeEntry(w, r, cached, StatusHit, key)
		return
	}

	// The read back failed — the entry was not storable, or the backend is
	// unhealthy. Serve the leader's copy anyway: availability must not depend on
	// the cache, only cost does.
	m.coalesced.Add(1)
	m.writeEntry(w, r, entry, StatusCoalesced, key)
}

// fill performs the single fetch for a key and stores the result.
//
// Everything here runs exactly once per key per flight, and the ordering within
// it is the guarantee: the entry is written and acknowledged before this returns,
// and returning is what releases the waiting requests.
func (m *Middleware) fill(r *http.Request, next http.Handler, policy Policy, key cacheKey) *cache.Entry {
	// Detach from the originating request's cancellation.
	//
	// If the client that happened to be elected leader disconnects, its request
	// context is cancelled. Inheriting that would abort the fetch, fail every
	// request waiting behind it, and waste a call that may already have been
	// billed. WithoutCancel keeps the request-scoped values — request id, logger
	// — while dropping the cancellation that does not belong to the followers.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), m.opts.FetchTimeout)
	defer cancel()

	// Re-check now that we hold the flight. Another leader may have completed
	// between our miss and our election.
	if entry, ok, err := m.cache.Get(ctx, key.hash); err == nil && ok {
		return entry
	}

	// Cross-instance coordination. Within this process singleflight has already
	// done its job; this extends the same guarantee across replicas.
	if release, acquired, err := m.locker.Acquire(ctx, key.hash); err != nil {
		// Fail open: an unreachable lock may cost a duplicate call, but blocking
		// on it would cost availability. The budget remains the real ceiling.
		m.log.WarnContext(ctx, "cache lock unavailable; proceeding without it",
			slog.String("key", key.readable), slog.String("error", err.Error()))
	} else if acquired {
		defer release(context.WithoutCancel(ctx))
	} else if entry, ok := m.waitForPeer(ctx, key); ok {
		// Another instance fetched it. That is one upstream call for the fleet.
		m.peerFills.Add(1)
		return entry
	} else {
		// The peer did not deliver in time. Proceed rather than fail: a slow or
		// dead leader must not turn into an outage.
		m.peerTimeouts.Add(1)
	}

	rec := newRecorder(m.opts.MaxBodyBytes)
	next.ServeHTTP(rec, r.WithContext(ctx))
	entry := rec.entry(m.opts.StoreHeaders)

	ttl, cacheable := policy.ttlFor(entry.Status)
	if !cacheable || !rec.cacheable() {
		return entry
	}

	// The critical ordering. Set is synchronous and its acknowledgement is
	// awaited, and both happen before this function returns. Moving this after
	// the return — or making it asynchronous — would let a released follower, or
	// a fast client's next request, observe an empty cache and fetch again.
	if err := m.cache.Set(ctx, key.hash, entry, ttl); err != nil {
		m.storeErrors.Add(1)
		m.log.WarnContext(ctx, "cache store failed; response will not be reused",
			slog.String("key", key.readable), slog.String("error", err.Error()))
		return entry
	}
	m.stored.Add(1)
	return entry
}

// waitForPeer polls the shared cache for a fill by another instance, giving up
// after the configured timeout. Bounded waiting is deliberate: a crashed peer
// holding a lock should cost latency, never availability.
func (m *Middleware) waitForPeer(ctx context.Context, key cacheKey) (*cache.Entry, bool) {
	if m.opts.LockWaitTimeout <= 0 {
		return nil, false
	}
	deadline := time.Now().Add(m.opts.LockWaitTimeout)
	ticker := time.NewTicker(m.opts.LockPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, false
		case <-ticker.C:
			if entry, ok, err := m.cache.Get(ctx, key.hash); err == nil && ok {
				return entry, true
			}
			if time.Now().After(deadline) {
				return nil, false
			}
		}
	}
}

// writeEntry replays a cached response.
func (m *Middleware) writeEntry(w http.ResponseWriter, r *http.Request, entry *cache.Entry, status string, key cacheKey) {
	header := w.Header()
	for name, values := range entry.Header {
		header[name] = values
	}
	header.Set("X-Cache", status)
	if status != StatusMiss {
		header.Set("X-Cache-Age", strconv.FormatInt(int64(entry.Age(time.Now()).Seconds()), 10))
	}
	if m.opts.DebugHeaders {
		header.Set("X-Cache-Key", key.readable)
	}

	// We hold a validator for every entry, so a repeat client can be answered
	// without resending the body. This saves bandwidth rather than upstream
	// cost, but it is free given the entry is already in hand.
	if etag := entry.Header.Get("ETag"); etag != "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
		header.Del("Content-Length")
		w.WriteHeader(http.StatusNotModified)
		return
	}

	header.Set("Content-Length", strconv.Itoa(len(entry.Body)))
	w.WriteHeader(entry.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(entry.Body)
	}
}

// etagMatches implements If-None-Match comparison. Weak comparison is correct
// here: the header is only used to decide whether to resend a body that the
// client already has.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	want := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == want {
			return true
		}
	}
	return false
}
