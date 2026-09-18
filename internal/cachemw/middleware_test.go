package cachemw

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/lock"
)

// Every test here drives the middleware through a stub handler that knows
// nothing about posts or todos. That is the point: if these tests needed the
// real service, the middleware would not be the generic component it claims to
// be.

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testMiddleware(t *testing.T) (*Middleware, cache.Cache) {
	t.Helper()
	backend := cache.NewMemory(cache.MemoryOptions{MaxEntries: 128, MaxNegativeEntries: 32})
	t.Cleanup(func() { _ = backend.Close() })

	mw := New(backend, lock.NoOp{}, Options{
		Enabled:      true,
		MaxBodyBytes: 1 << 20,
		StoreHeaders: []string{"Content-Type", "ETag"},
		FetchTimeout: 5 * time.Second,
	}, discardLogger())
	return mw, backend
}

func immutablePolicy() Policy {
	return Policy{Name: "test", Enabled: true, TTL: 0, NegativeTTL: 5 * time.Minute}
}

// countingHandler records how many times it ran. In production this stands in
// for the billable upstream call, so its counter is the number the entire design
// exists to hold down.
func countingHandler(calls *atomic.Int64, body string, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// TestSingleFlightConcurrentColdKey is the direct proof of the bonus
// requirement.
//
// 200 requests for the same uncached key are released simultaneously while the
// handler is held open, so every one of them reaches the middleware before the
// first has produced a response. A plain read-through cache fails this outright:
// all 200 would miss and all 200 would call upstream.
//
// The assertion on the call count cannot fail spuriously. Late-arriving
// goroutines can only find the entry already cached, which lowers the count;
// nothing except a genuine breakdown of coalescing can raise it above one.
func TestSingleFlightConcurrentColdKey(t *testing.T) {
	t.Parallel()

	const concurrency = 200

	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release // hold the leader until every follower has parked behind it
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})

	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(handler)

	start := make(chan struct{})
	results := make([]*httptest.ResponseRecorder, concurrency)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := range concurrency {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			<-start // barrier: all goroutines enter the middleware together
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
			results[i] = rec
		}()
	}

	close(start)
	<-entered                          // the leader is inside the handler
	time.Sleep(100 * time.Millisecond) // give the other 199 time to pile up
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream was called %d times for %d concurrent requests; want exactly 1", got, concurrency)
	}

	var hits, misses int
	for i, rec := range results {
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
		if body := rec.Body.String(); body != `{"id":1}` {
			t.Fatalf("request %d: body = %q, want the cached payload", i, body)
		}
		switch rec.Header().Get("X-Cache") {
		case StatusHit, StatusCoalesced:
			hits++
		case StatusMiss:
			misses++
		}
	}

	if misses != 1 {
		t.Errorf("X-Cache: MISS count = %d, want exactly 1", misses)
	}
	if hits != concurrency-1 {
		t.Errorf("cache-served count = %d, want %d", hits, concurrency-1)
	}
}

// TestCacheIsPopulatedBeforeResponseIsFlushed proves the ordering guarantee.
//
// The moment the leader's response begins to be written, a second request is
// issued through the same middleware — simulating a client that receives an
// answer and immediately asks again. It must be a cache hit. If the store
// happened after the flush instead of before it, that second request would miss
// and trigger a second billable call, which is the race this ordering exists to
// close.
func TestCacheIsPopulatedBeforeResponseIsFlushed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"id":1}`, http.StatusOK))

	var secondCacheStatus string
	var secondCode int

	probe := &firstWriteProbe{
		ResponseWriter: httptest.NewRecorder(),
		onFirstWrite: func() {
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
			secondCacheStatus = rec.Header().Get("X-Cache")
			secondCode = rec.Code
		},
	}

	wrapped.ServeHTTP(probe, httptest.NewRequest(http.MethodGet, "/posts/1", nil))

	if secondCode != http.StatusOK {
		t.Fatalf("follow-up request status = %d, want 200", secondCode)
	}
	if secondCacheStatus != StatusHit {
		t.Fatalf("a request made while the first response was still being written got X-Cache=%q, want %q: "+
			"the entry must be stored before any byte is flushed", secondCacheStatus, StatusHit)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times; want 1", got)
	}
}

// firstWriteProbe runs a callback at the exact moment the response starts being
// written, whether that is a WriteHeader or a Write.
type firstWriteProbe struct {
	http.ResponseWriter
	onFirstWrite func()
	once         sync.Once
}

func (p *firstWriteProbe) WriteHeader(status int) {
	p.once.Do(p.onFirstWrite)
	p.ResponseWriter.WriteHeader(status)
}

func (p *firstWriteProbe) Write(b []byte) (int, error) {
	p.once.Do(p.onFirstWrite)
	return p.ResponseWriter.Write(b)
}

// TestLeaderCancellationDoesNotFailFollowers covers a failure mode that is easy
// to ship by accident.
//
// The request elected as leader is cancelled — its client disconnected. If the
// fetch inherited that cancellation it would abort, every request waiting behind
// it would fail, and a call that may already have been billed would be thrown
// away. The fetch must therefore run on a context detached from the leader's.
func TestLeaderCancellationDoesNotFailFollowers(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	var handlerCtxErr error

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		// Recorded after the leader's context was cancelled: it must still be
		// live here, because it belongs to the fill, not to one caller.
		handlerCtxErr = r.Context().Err()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})

	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(handler)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		req := httptest.NewRequest(http.MethodGet, "/posts/1", nil).WithContext(leaderCtx)
		wrapped.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-entered

	const followers = 20
	recs := make([]*httptest.ResponseRecorder, followers)
	var wg sync.WaitGroup
	wg.Add(followers)
	for i := range followers {
		go func() {
			defer wg.Done()
			recs[i] = httptest.NewRecorder()
			wrapped.ServeHTTP(recs[i], httptest.NewRequest(http.MethodGet, "/posts/1", nil))
		}()
	}

	time.Sleep(50 * time.Millisecond)
	cancelLeader() // the leader's client goes away mid-fetch
	time.Sleep(50 * time.Millisecond)
	close(release)

	wg.Wait()
	<-leaderDone

	if handlerCtxErr != nil {
		t.Fatalf("handler context was cancelled (%v); the fill must be detached from the leader's request", handlerCtxErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times; want 1", got)
	}
	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Errorf("follower %d: status = %d, want 200 despite the leader disconnecting", i, rec.Code)
		}
	}
}

// TestSequentialRequestsServedFromCache covers the plain repetition case: after
// the first request, every later one is answered without touching the handler.
func TestSequentialRequestsServedFromCache(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"id":1}`, http.StatusOK))

	for i := range 25 {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))

		want := StatusHit
		if i == 0 {
			want = StatusMiss
		}
		if got := rec.Header().Get("X-Cache"); got != want {
			t.Fatalf("request %d: X-Cache = %q, want %q", i, got, want)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times across 25 requests; want 1", got)
	}
}

// TestNegativeResponsesAreCached matters for cost: the upstream bills for a 404
// exactly as it bills for a hit, so a loop over a missing id would otherwise be
// unbounded spend.
func TestNegativeResponsesAreCached(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"error":"nope"}`, http.StatusNotFound))

	for range 10 {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/404", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times for a repeated 404; want 1", got)
	}
}

// TestNegativeTTLZeroDisablesNegativeCaching documents the deliberate asymmetry
// with the positive TTL, where zero means "never expires".
func TestNegativeTTLZeroDisablesNegativeCaching(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	policy := Policy{Name: "test", Enabled: true, TTL: 0, NegativeTTL: 0}
	wrapped := mw.For(policy)(countingHandler(&calls, `{}`, http.StatusNotFound))

	for range 3 {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/404", nil))
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times; want 3 (negative caching disabled)", got)
	}
}

// TestServerErrorsAreNotCached: a 500 is transient, and storing it would serve a
// failure long after the cause was fixed.
func TestServerErrorsAreNotCached(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"error":"boom"}`, http.StatusInternalServerError))

	for range 3 {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times; want 3 (5xx must never be cached)", got)
	}
}

// TestDisabledPolicyBypassesCache is the per-route switch the requirements ask
// for: the same middleware, mounted identically, caching nothing.
func TestDisabledPolicyBypassesCache(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(Policy{Name: "off", Enabled: false})(countingHandler(&calls, `{"id":1}`, http.StatusOK))

	for range 5 {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
		if got := rec.Header().Get("X-Cache"); got != StatusBypass {
			t.Fatalf("X-Cache = %q, want %q", got, StatusBypass)
		}
	}

	if got := calls.Load(); got != 5 {
		t.Fatalf("upstream called %d times; want 5 (caching disabled for this route)", got)
	}
}

// TestNonGETIsNotCached: a cached response for a method that may have side
// effects would be wrong regardless of policy.
func TestNonGETIsNotCached(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"id":1}`, http.StatusOK))

	for range 3 {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/posts/1", nil))
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("POST was called upstream %d times; want 3 (never cached)", got)
	}
}

// TestQueryOrderDoesNotAffectKey: the requirement is "the same parameters", not
// "the same string".
func TestQueryOrderDoesNotAffectKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `[]`, http.StatusOK))

	for _, target := range []string{
		"/todos?userId=1&completed=true",
		"/todos?completed=true&userId=1",
	} {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times for two orderings of one query; want 1", got)
	}
}

// TestDifferentParametersAreDifferentEntries is the converse: coalescing must
// not go so far as to conflate distinct requests.
func TestDifferentParametersAreDifferentEntries(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `[]`, http.StatusOK))

	for _, target := range []string{"/todos?userId=1", "/todos?userId=2", "/todos"} {
		wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times for three distinct queries; want 3", got)
	}
}

// TestConditionalRequestReturns304: entries carry a validator, so a client that
// already holds the body can be told so instead of being sent it again.
func TestConditionalRequestReturns304(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, `{"id":1}`, http.StatusOK))

	first := httptest.NewRecorder()
	wrapped.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag was generated for the cached entry")
	}

	second := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/posts/1", nil)
	req.Header.Set("If-None-Match", etag)
	wrapped.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Errorf("304 response carried a %d-byte body; want none", second.Body.Len())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times; want 1", got)
	}
}

// TestOversizedResponseIsServedButNotCached: the ordering guarantee requires
// buffering, so an entry too large to hold is served and then dropped rather
// than retained.
func TestOversizedResponseIsServedButNotCached(t *testing.T) {
	t.Parallel()

	backend := cache.NewMemory(cache.MemoryOptions{MaxEntries: 16, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = backend.Close() })

	mw := New(backend, lock.NoOp{}, Options{
		Enabled:      true,
		MaxBodyBytes: 32, // smaller than the payload below
		StoreHeaders: []string{"Content-Type"},
		FetchTimeout: time.Second,
	}, discardLogger())

	big := make([]byte, 256)
	for i := range big {
		big[i] = 'x'
	}

	var calls atomic.Int64
	wrapped := mw.For(immutablePolicy())(countingHandler(&calls, string(big), http.StatusOK))

	for range 3 {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
		if rec.Body.Len() != len(big) {
			t.Fatalf("body length = %d, want %d: oversized responses must still be served in full", rec.Body.Len(), len(big))
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream called %d times; want 3 (oversized entries are not cached)", got)
	}
}

// TestPanicInLeaderDoesNotCrashFollowers documents why Recoverer must sit
// outside this middleware: singleflight re-panics in every waiter.
func TestPanicInLeaderDoesNotCrashFollowers(t *testing.T) {
	t.Parallel()

	mw, _ := testMiddleware(t)
	wrapped := mw.For(immutablePolicy())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	}))

	var recovered atomic.Int64
	var wg sync.WaitGroup
	wg.Add(10)
	for range 10 {
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					recovered.Add(1)
				}
			}()
			wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/1", nil))
		}()
	}
	wg.Wait()

	if recovered.Load() == 0 {
		t.Fatal("expected the panic to surface to callers so an outer Recoverer can handle it")
	}
}
