package httpmw

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRequestIDIsGeneratedAndPropagated(t *testing.T) {
	t.Parallel()

	var seen string
	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Error("no request id in the handler context")
	}
	if got := rec.Header().Get(RequestIDHeader); got != seen {
		t.Errorf("response header = %q, context = %q; they must match", got, seen)
	}
}

func TestRequestIDPrefersCallerSupplied(t *testing.T) {
	t.Parallel()

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "trace-me")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != "trace-me" {
		t.Errorf("id = %q, want the caller's id preserved so a trace spans hops", got)
	}
}

// TestOversizedRequestIDIsReplaced: request ids reach the logs, so an unbounded
// client-controlled string must not.
func TestOversizedRequestIDIsReplaced(t *testing.T) {
	t.Parallel()

	handler := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, strings.Repeat("A", 500))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); len(got) > 128 {
		t.Errorf("id length = %d, want a regenerated bounded id", len(got))
	}
}

func TestRecovererConvertsPanicToServerError(t *testing.T) {
	t.Parallel()

	handler := Recoverer(quietLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "internal_error") {
		t.Errorf("body = %q, want the standard error envelope", body)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value must not be exposed to the client")
	}
}

// TestRecovererRespectsErrAbortHandler: that sentinel is how a handler
// deliberately abandons a response, so it must not be logged as a fault.
func TestRecovererRespectsErrAbortHandler(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want ErrAbortHandler to propagate", r)
		}
	}()

	handler := Recoverer(quietLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestLoggerPassesResponsesThrough(t *testing.T) {
	t.Parallel()

	handler := Logger(quietLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("body"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want 418 passed through unchanged", rec.Code)
	}
	if rec.Body.String() != "body" {
		t.Errorf("body = %q, want it passed through unchanged", rec.Body.String())
	}
}

func TestTimeoutAppliesADeadline(t *testing.T) {
	t.Parallel()

	var hadDeadline bool
	handler := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadDeadline = r.Context().Deadline()
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !hadDeadline {
		t.Error("the handler context should carry a deadline")
	}
}

func TestRateLimiterAllowsBurstThenRejects(t *testing.T) {
	t.Parallel()

	var rejected int
	limiter := NewRateLimiter(RateLimitOptions{
		RequestsPerSecond: 1,
		Burst:             3,
		OnReject: func(w http.ResponseWriter, r *http.Request) {
			rejected++
			w.WriteHeader(http.StatusTooManyRequests)
		},
	})

	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for range 10 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	if rejected == 0 {
		t.Error("expected requests beyond the burst to be rejected")
	}
	if rejected > 7 {
		t.Errorf("rejected %d of 10; the burst of 3 should have been admitted", rejected)
	}
}

func TestRateLimiterIsPerClient(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(RateLimitOptions{
		RequestsPerSecond: 1, Burst: 2,
		OnReject: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
	})
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust one client.
	for range 5 {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.1:1111"
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// A different client must be unaffected.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:2222"
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("second client got %d; limits must be per client", rec.Code)
	}
}

// TestProxyHeadersAreIgnoredUnlessTrusted: X-Forwarded-For is trivially forged,
// so honouring it by default would let any client mint a fresh identity per
// request and defeat the limiter entirely.
func TestProxyHeadersAreIgnoredUnlessTrusted(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1111"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")

	if got := clientIP(req, false); got != "10.0.0.1" {
		t.Errorf("untrusted: clientIP = %q, want the socket address 10.0.0.1", got)
	}
	if got := clientIP(req, true); got != "1.2.3.4" {
		t.Errorf("trusted: clientIP = %q, want the forwarded address", got)
	}
}

func TestForwardedForUsesLeftmostEntry(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1111"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 10.0.0.9, 10.0.0.8")

	if got := clientIP(req, true); got != "1.2.3.4" {
		t.Errorf("clientIP = %q, want the original client rather than an intermediary", got)
	}
}

func TestRateLimiterBoundsItsOwnMemory(t *testing.T) {
	t.Parallel()

	limiter := NewRateLimiter(RateLimitOptions{
		RequestsPerSecond: 100, Burst: 100, MaxClients: 50,
		OnReject: func(w http.ResponseWriter, r *http.Request) {},
	})

	// The limiter is keyed by an attacker-influenced value, so its map must be
	// bounded or the protection becomes its own denial-of-service vector.
	for i := range 500 {
		limiter.allow(string(rune(i)) + "-client")
	}

	limiter.mu.Lock()
	size := len(limiter.clients)
	limiter.mu.Unlock()

	if size > 50 {
		t.Errorf("limiter holds %d clients, exceeding its bound of 50", size)
	}
}
