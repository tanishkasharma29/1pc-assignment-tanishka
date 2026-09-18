package httpmw

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitOptions configures the per-client limiter.
type RateLimitOptions struct {
	RequestsPerSecond float64
	Burst             int
	// MaxClients bounds the limiter's own memory. The limiter is keyed by client
	// address, which is attacker-influenced, so an unbounded map would turn a
	// protection into a denial-of-service vector of its own.
	MaxClients int
	ClientTTL  time.Duration
	// TrustProxyHeaders makes the limiter read X-Forwarded-For. Enable it only
	// behind a proxy you control: otherwise any client can present a fresh
	// identity per request and the limit stops existing.
	TrustProxyHeaders bool
	OnReject          func(http.ResponseWriter, *http.Request)
}

// RateLimiter applies a token bucket per client address.
//
// This bounds what any single caller can cost us. It is not, on its own, a bound
// on total spend — many clients within their limits still add up — which is why
// the upstream budget exists as a separate and absolute ceiling.
type RateLimiter struct {
	opts RateLimitOptions

	mu      sync.Mutex
	clients map[string]*clientLimiter
}

type clientLimiter struct {
	limiter *rate.Limiter
	seen    time.Time
}

func NewRateLimiter(opts RateLimitOptions) *RateLimiter {
	if opts.MaxClients <= 0 {
		opts.MaxClients = 10000
	}
	if opts.ClientTTL <= 0 {
		opts.ClientTTL = 10 * time.Minute
	}
	if opts.Burst <= 0 {
		opts.Burst = 1
	}
	return &RateLimiter{opts: opts, clients: make(map[string]*clientLimiter)}
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientIP(r, rl.opts.TrustProxyHeaders)) {
			rl.opts.OnReject(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	client, ok := rl.clients[key]
	if !ok {
		// Evict before admitting, so the map can never exceed its bound.
		if len(rl.clients) >= rl.opts.MaxClients {
			rl.evictLocked(now)
		}
		client = &clientLimiter{limiter: rate.NewLimiter(rate.Limit(rl.opts.RequestsPerSecond), rl.opts.Burst)}
		rl.clients[key] = client
	}
	client.seen = now
	return client.limiter.Allow()
}

// evictLocked drops idle clients, and if none are idle, drops an arbitrary entry
// so admission always succeeds. Losing a limiter is harmless: the client simply
// starts with a full bucket.
func (rl *RateLimiter) evictLocked(now time.Time) {
	for key, client := range rl.clients {
		if now.Sub(client.seen) > rl.opts.ClientTTL {
			delete(rl.clients, key)
		}
	}
	if len(rl.clients) < rl.opts.MaxClients {
		return
	}
	for key := range rl.clients {
		delete(rl.clients, key)
		return
	}
}

// clientIP identifies the caller. Proxy headers are consulted only when
// explicitly trusted, because they are trivially forged.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			// The left-most entry is the original client; the rest were added by
			// intermediaries.
			if comma := indexByte(forwarded, ','); comma > 0 {
				return trimSpace(forwarded[:comma])
			}
			return trimSpace(forwarded)
		}
		if realIP := r.Header.Get("X-Real-Ip"); realIP != "" {
			return trimSpace(realIP)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
