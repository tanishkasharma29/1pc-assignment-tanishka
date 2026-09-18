package httpapi_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/cachemw"
	"github.com/lemonn/typecode-middleware/internal/config"
	"github.com/lemonn/typecode-middleware/internal/lock"
	"github.com/lemonn/typecode-middleware/internal/service"
	"github.com/lemonn/typecode-middleware/internal/transport/httpapi"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

// fakeTypicode reproduces the upstream behaviours observed by probing the real
// API, including the awkward ones: malformed ids come back as a billable 404
// rather than a cheap rejection, unknown query parameters are ignored, and
// _limit is genuinely honoured.
func fakeTypicode(calls *atomic.Int64) http.Handler {
	post := func(id int) string {
		return fmt.Sprintf(`{"userId":%d,"id":%d,"title":"title %d","body":"body %d"}`,
			(id-1)/10+1, id, id, id)
	}
	todo := func(id int) string {
		return fmt.Sprintf(`{"userId":%d,"id":%d,"title":"todo %d","completed":%t}`,
			(id-1)/20+1, id, id, id%2 == 0)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-RateLimit-Limit", "1000")
		w.Header().Set("X-RateLimit-Remaining", "999")

		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		resource := parts[0]

		maxID := map[string]int{"posts": 100, "todos": 200}[resource]
		if maxID == 0 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
			return
		}

		if len(parts) == 2 {
			id, err := strconv.Atoi(parts[1])
			if err != nil || id < 1 || id > maxID {
				// Exactly what the real upstream does: a paid 404, not a 400.
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{}`))
				return
			}
			if resource == "posts" {
				_, _ = w.Write([]byte(post(id)))
			} else {
				_, _ = w.Write([]byte(todo(id)))
			}
			return
		}

		limit := maxID
		if raw := r.URL.Query().Get("_limit"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				limit = n // honoured, which is why we reject it at our edge
			}
		}
		userFilter, hasUser := 0, false
		if raw := r.URL.Query().Get("userId"); raw != "" {
			userFilter, _ = strconv.Atoi(raw)
			hasUser = true
		}

		items := make([]string, 0, limit)
		for id := 1; id <= maxID && len(items) < limit; id++ {
			owner := (id-1)/10 + 1
			body := post(id)
			if resource == "todos" {
				owner = (id-1)/20 + 1
				body = todo(id)
			}
			if hasUser && owner != userFilter {
				continue
			}
			items = append(items, body)
		}
		_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	})
}

type testAPI struct {
	server        *httptest.Server
	upstreamCalls *atomic.Int64
}

func (a *testAPI) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := a.server.Client().Get(a.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func newTestAPI(t *testing.T, tune func(*config.Config)) *testAPI {
	t.Helper()

	calls := &atomic.Int64{}
	fake := httptest.NewServer(fakeTypicode(calls))
	t.Cleanup(fake.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := config.Default()
	cfg.Upstream.BaseURL = fake.URL
	cfg.RateLimit.Enabled = false // exercised separately; noise here
	if tune != nil {
		tune(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	up, err := upstream.New(cfg.Upstream, log)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	backend := cache.NewMemory(cache.MemoryOptions{
		MaxEntries:         cfg.Cache.Memory.MaxEntries,
		MaxNegativeEntries: cfg.Cache.Memory.MaxNegativeEntries,
		MaxBytes:           cfg.Cache.Memory.MaxBytes,
	})
	t.Cleanup(func() { _ = backend.Close() })

	mw := cachemw.New(backend, lock.NoOp{}, cachemw.Options{
		Enabled:      cfg.Cache.Enabled,
		MaxBodyBytes: cfg.Cache.MaxBodyBytes,
		StoreHeaders: cfg.Cache.StoreHeaders,
		FetchTimeout: cfg.Server.RequestTimeout.Std(),
	}, log)

	api := httpapi.NewServer(cfg, log, service.New(up, log), mw, backend, up)
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	return &testAPI{server: srv, upstreamCalls: calls}
}

func TestItemIsFetchedOnceAndThenCached(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	for _, path := range []string{"/posts/1", "/todos/1"} {
		for i := range 5 {
			resp, body := api.get(t, path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status = %d", path, resp.StatusCode)
			}
			want := cachemw.StatusHit
			if i == 0 {
				want = cachemw.StatusMiss
			}
			if got := resp.Header.Get("X-Cache"); got != want {
				t.Errorf("%s request %d: X-Cache = %q, want %q", path, i, got, want)
			}
			if !strings.Contains(body, `"id":1`) {
				t.Errorf("%s: unexpected body %q", path, body)
			}
		}
	}

	if got := api.upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one per distinct resource)", got)
	}
}

// TestRejectedRequestsCostNothing is the cheapest saving in the design.
//
// The upstream answers /posts/abc, /posts/0 and /posts/-1 with a billable 404
// rather than rejecting them cheaply, so refusing them at our edge converts a
// paid round trip into a local 400. The assertion is that the fake upstream is
// never contacted at all.
func TestRejectedRequestsCostNothing(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	rejected := []string{
		"/posts/abc", "/posts/0", "/posts/-1", "/posts/1.5", "/posts/%20",
		"/todos/abc", "/todos/0", "/todos/-7",
		"/posts/99999999", // beyond the configured id bound
		"/posts?_limit=5", // honoured upstream, declined here
		"/posts?_page=2",
		"/todos?completed=yes", // not a value the upstream understands
		"/posts?userId=0",
		"/posts?userId=abc",
		"/posts?userId=1&userId=2", // ambiguous repetition
	}

	for _, path := range rejected {
		resp, body := api.get(t, path)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body %s)", path, resp.StatusCode, body)
		}
		if !strings.Contains(body, `"code":"bad_request"`) {
			t.Errorf("%s: body = %s, want a bad_request envelope", path, body)
		}
	}

	if got := api.upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream was called %d times for %d invalid requests; want 0", got, len(rejected))
	}
}

// TestUnknownParametersShareTheBareRouteEntry.
//
// The upstream ignores unrecognised parameters and returns the identical
// response, so stripping them is both faithful and the thing that stops
// ?x=1, ?x=2, ?x=3 ... from minting unlimited cache keys and unlimited calls.
func TestUnknownParametersShareTheBareRouteEntry(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	first, bare := api.get(t, "/posts")
	if got := first.Header.Get("X-Cache"); got != cachemw.StatusMiss {
		t.Fatalf("first /posts: X-Cache = %q, want MISS", got)
	}

	for _, path := range []string{"/posts?foo=bar", "/posts?utm_source=x&ref=y", "/posts?cachebust=999"} {
		resp, body := api.get(t, path)
		if got := resp.Header.Get("X-Cache"); got != cachemw.StatusHit {
			t.Errorf("%s: X-Cache = %q, want HIT on the bare /posts entry", path, got)
		}
		if body != bare {
			t.Errorf("%s: body differs from /posts; stripping must not change the answer", path)
		}
	}

	if got := api.upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1: cache-busting parameters must not generate calls", got)
	}
}

func TestNotFoundIsCached(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	for i := range 5 {
		resp, body := api.get(t, "/posts/101") // within our bound, absent upstream
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if !strings.Contains(body, `"code":"not_found"`) {
			t.Errorf("request %d: body = %s", i, body)
		}
	}

	if got := api.upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1: a repeated 404 must be paid for once", got)
	}
}

func TestCollectionFilteringAndCacheKeys(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	resp, body := api.get(t, "/todos?userId=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var todos []map[string]any
	if err := json.Unmarshal([]byte(body), &todos); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(todos) != 20 {
		t.Errorf("got %d todos for userId=1, want 20", len(todos))
	}

	// Reordering the query must not produce a second entry.
	api.get(t, "/todos?userId=1&completed=true")
	resp2, _ := api.get(t, "/todos?completed=true&userId=1")
	if got := resp2.Header.Get("X-Cache"); got != cachemw.StatusHit {
		t.Errorf("reordered query: X-Cache = %q, want HIT", got)
	}

	// A genuinely different filter must produce a different entry.
	resp3, _ := api.get(t, "/todos?userId=2")
	if got := resp3.Header.Get("X-Cache"); got != cachemw.StatusMiss {
		t.Errorf("different filter: X-Cache = %q, want MISS", got)
	}
}

// TestConcurrentColdRequestsThroughTheFullStack repeats the middleware's
// coalescing proof end-to-end, so the guarantee is shown to survive routing,
// validation, the service layer and the upstream client.
func TestConcurrentColdRequestsThroughTheFullStack(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	const concurrency = 150
	var wg sync.WaitGroup
	statuses := make([]int, concurrency)
	cacheHeaders := make([]string, concurrency)
	start := make(chan struct{})

	wg.Add(concurrency)
	for i := range concurrency {
		go func() {
			defer wg.Done()
			<-start
			resp, err := api.server.Client().Get(api.server.URL + "/posts/42")
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			statuses[i] = resp.StatusCode
			cacheHeaders[i] = resp.Header.Get("X-Cache")
		}()
	}
	close(start)
	wg.Wait()

	if got := api.upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d for %d concurrent cold requests; want exactly 1", got, concurrency)
	}

	misses := 0
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, status)
		}
		if cacheHeaders[i] == cachemw.StatusMiss {
			misses++
		}
	}
	if misses != 1 {
		t.Errorf("MISS count = %d, want 1", misses)
	}
}

func TestCachingCanBeDisabledPerRoute(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, func(cfg *config.Config) {
		off := false
		policy := cfg.Cache.Routes[config.RoutePostsItem]
		policy.Enabled = &off
		cfg.Cache.Routes[config.RoutePostsItem] = policy
	})

	for range 3 {
		resp, _ := api.get(t, "/posts/1")
		if got := resp.Header.Get("X-Cache"); got != cachemw.StatusBypass {
			t.Errorf("X-Cache = %q, want %q", got, cachemw.StatusBypass)
		}
	}
	// The sibling route keeps its own policy: this is per-route, not global.
	for i := range 3 {
		resp, _ := api.get(t, "/todos/1")
		want := cachemw.StatusHit
		if i == 0 {
			want = cachemw.StatusMiss
		}
		if got := resp.Header.Get("X-Cache"); got != want {
			t.Errorf("/todos/1 request %d: X-Cache = %q, want %q", i, got, want)
		}
	}

	if got := api.upstreamCalls.Load(); got != 4 {
		t.Fatalf("upstream calls = %d, want 4 (3 uncached posts + 1 todo)", got)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	resp, body := api.get(t, "/comments/1")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("unknown route: status = %d body = %s", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodDelete, api.server.URL+"/posts/1", nil)
	del, err := api.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE: status = %d, want 400", del.StatusCode)
	}
	if got := api.upstreamCalls.Load(); got != 0 {
		t.Errorf("upstream calls = %d, want 0", got)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	health, _ := api.get(t, "/healthz")
	if health.StatusCode != http.StatusOK {
		t.Errorf("healthz: status = %d", health.StatusCode)
	}

	api.get(t, "/posts/1")
	api.get(t, "/posts/1")

	ready, body := api.get(t, "/readyz")
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("readyz: status = %d", ready.StatusCode)
	}
	var payload struct {
		Status string `json:"status"`
		Cache  struct {
			Hits     uint64  `json:"hits"`
			Misses   uint64  `json:"misses"`
			HitRatio float64 `json:"hit_ratio"`
		} `json:"cache"`
		UpstreamCalls int64 `json:"upstream_calls"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode readyz: %v (%s)", err, body)
	}
	if payload.Cache.Hits != 1 || payload.Cache.Misses != 1 {
		t.Errorf("hits=%d misses=%d, want 1 and 1", payload.Cache.Hits, payload.Cache.Misses)
	}
	if payload.UpstreamCalls != 1 {
		t.Errorf("upstream_calls = %d, want 1", payload.UpstreamCalls)
	}
}

func TestRequestIDIsAlwaysReturned(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, nil)

	resp, _ := api.get(t, "/posts/1")
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("no X-Request-Id on the response")
	}

	req, _ := http.NewRequest(http.MethodGet, api.server.URL+"/posts/2", nil)
	req.Header.Set("X-Request-Id", "caller-supplied-id")
	got, err := api.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	if id := got.Header.Get("X-Request-Id"); id != "caller-supplied-id" {
		t.Errorf("X-Request-Id = %q, want the caller's id to be preserved for tracing", id)
	}
}

func TestRateLimiterRejectsFloods(t *testing.T) {
	t.Parallel()
	api := newTestAPI(t, func(cfg *config.Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.RequestsPerSecond = 1
		cfg.RateLimit.Burst = 3
	})

	var limited int
	for range 25 {
		resp, _ := api.get(t, "/posts/1")
		if resp.StatusCode == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Error("expected some requests to be rate limited")
	}
}

func TestUpstreamFailureIsNotCached(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(broken.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := config.Default()
	cfg.Upstream.BaseURL = broken.URL
	cfg.Upstream.MaxAttempts = 1
	cfg.Upstream.Timeout = config.Duration(time.Second)
	cfg.RateLimit.Enabled = false

	up, _ := upstream.New(cfg.Upstream, log)
	backend := cache.NewMemory(cache.MemoryOptions{MaxEntries: 32, MaxNegativeEntries: 8})
	t.Cleanup(func() { _ = backend.Close() })
	mw := cachemw.New(backend, lock.NoOp{}, cachemw.Options{
		Enabled: true, MaxBodyBytes: 1 << 20,
		StoreHeaders: cfg.Cache.StoreHeaders, FetchTimeout: 5 * time.Second,
	}, log)
	srv := httptest.NewServer(httpapi.NewServer(cfg, log, service.New(up, log), mw, backend, up).Routes())
	t.Cleanup(srv.Close)

	for range 3 {
		resp, err := srv.Client().Get(srv.URL + "/posts/1")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3: a 5xx must never be cached and served later", got)
	}
}
