package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testClient(t *testing.T, baseURL string, tune func(*config.Upstream)) *Client {
	t.Helper()
	cfg := config.Default().Upstream
	cfg.BaseURL = baseURL
	cfg.RetryBaseDelay = config.Duration(time.Millisecond)
	cfg.RetryMaxDelay = config.Duration(5 * time.Millisecond)
	if tune != nil {
		tune(&cfg)
	}
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/posts/1" {
			t.Errorf("path = %q, want /posts/1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, nil)
	resp, err := c.Get(context.Background(), "/posts/1", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status != http.StatusOK || string(resp.Body) != `{"id":1}` {
		t.Fatalf("status=%d body=%q", resp.Status, resp.Body)
	}
	if c.Calls() != 1 {
		t.Errorf("Calls() = %d, want 1", c.Calls())
	}
}

func TestQueryIsForwarded(t *testing.T) {
	t.Parallel()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, nil)
	if _, err := c.Get(context.Background(), "/todos", url.Values{"userId": {"1"}}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if seen != "userId=1" {
		t.Errorf("upstream saw query %q, want userId=1", seen)
	}
}

func TestRetriesTransientServerErrors(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, nil)
	resp, err := c.Get(context.Background(), "/posts/1", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retries", resp.Status)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestDoesNotRetryNotFound: for immutable data a 404 is a deterministic answer.
// Retrying it would spend money to be told the same thing again.
func TestDoesNotRetryNotFound(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, nil)
	resp, err := c.Get(context.Background(), "/posts/99999", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 returned as a normal outcome", resp.Status)
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (a 404 must not be retried)", got)
	}
}

func TestGivesUpAfterMaxAttempts(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, func(u *config.Upstream) { u.MaxAttempts = 3 })
	resp, err := c.Get(context.Background(), "/posts/1", nil)
	if err != nil {
		t.Fatalf("Get returned an error; the final response should be handed back: %v", err)
	}
	if resp.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.Status)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want exactly max_attempts (3)", got)
	}
}

func TestRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, func(u *config.Upstream) {
		u.MaxResponseBytes = 512
		u.MaxAttempts = 1
	})
	if _, err := c.Get(context.Background(), "/posts/1", nil); err == nil {
		t.Fatal("expected an error for a response exceeding max_response_bytes")
	}
}

func TestTimeoutIsReportedDistinctly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, func(u *config.Upstream) {
		u.Timeout = config.Duration(20 * time.Millisecond)
		u.MaxAttempts = 1
	})

	_, err := c.Get(context.Background(), "/posts/1", nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusGatewayTimeout {
		t.Fatalf("error = %v; want a 504-mapped apperror so latency is distinguishable from breakage", err)
	}
}

// TestBudgetStopsSpending proves the only true ceiling on cost.
func TestBudgetStopsSpending(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, func(u *config.Upstream) {
		u.Budget.Enabled = true
		u.Budget.MaxCalls = 3
		u.Budget.Window = config.Duration(time.Hour)
	})

	for i := range 3 {
		if _, err := c.Get(context.Background(), "/posts/1", nil); err != nil {
			t.Fatalf("call %d should have been within budget: %v", i, err)
		}
	}
	_, err := c.Get(context.Background(), "/posts/1", nil)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v, want ErrBudgetExhausted", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("upstream saw %d calls, want 3: a refused call must never reach the wire", got)
	}
}

// TestBudgetHonoursUpstreamPublishedAllowance: the ceiling is grounded in what
// the provider actually permits, not a number invented at design time.
func TestBudgetHonoursUpstreamPublishedAllowance(t *testing.T) {
	t.Parallel()
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "1000")
		w.Header().Set("X-RateLimit-Remaining", "3") // below our reserve
		w.Header().Set("X-RateLimit-Reset", reset)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := testClient(t, srv.URL, func(u *config.Upstream) {
		u.Budget.Enabled = true
		u.Budget.MaxCalls = 10000 // our own limit is nowhere near
		u.Budget.ReserveRemaining = 10
	})

	if _, err := c.Get(context.Background(), "/posts/1", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Having learned the upstream is nearly out, we stop before it is.
	if _, err := c.Get(context.Background(), "/posts/2", nil); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("error = %v, want ErrBudgetExhausted once the upstream's own allowance ran low", err)
	}

	state := c.Budget().Snapshot()
	if state.UpstreamRemaining != 3 {
		t.Errorf("UpstreamRemaining = %d, want 3", state.UpstreamRemaining)
	}
}

func TestBudgetWindowResets(t *testing.T) {
	t.Parallel()
	b := NewBudget(true, 2, 50*time.Millisecond, 0)
	if !b.Allow() || !b.Allow() {
		t.Fatal("first two calls should be allowed")
	}
	if b.Allow() {
		t.Fatal("third call should exceed the budget")
	}
	time.Sleep(80 * time.Millisecond)
	if !b.Allow() {
		t.Error("budget should reset once the window elapses")
	}
}

func TestDisabledBudgetAlwaysAllows(t *testing.T) {
	t.Parallel()
	b := NewBudget(false, 1, time.Hour, 0)
	for i := range 100 {
		if !b.Allow() {
			t.Fatalf("call %d refused although the budget is disabled", i)
		}
	}
}
