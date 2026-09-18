package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/config"
	"github.com/lemonn/typecode-middleware/internal/service"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

func newService(t *testing.T, handler http.HandlerFunc, tune func(*config.Upstream)) *service.Service {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.MaxAttempts = 1
	cfg.Timeout = config.Duration(2 * time.Second)
	if tune != nil {
		tune(&cfg)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	up, err := upstream.New(cfg, log)
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	return service.New(up, log)
}

func TestPostIsDecoded(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"userId":1,"id":1,"title":"t","body":"b"}`))
	}, nil)

	post, err := svc.Post(context.Background(), 1)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if post.ID != 1 || post.Title != "t" {
		t.Fatalf("post = %+v", post)
	}
}

// TestNotFoundBecomesATypedError: the upstream answers a missing id with 404 and
// a bare `{}` body, which must become a proper application error rather than
// being decoded into a zero-valued resource.
func TestNotFoundBecomesATypedError(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}, nil)

	_, err := svc.Post(context.Background(), 999)
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, want a 404 apperror", err)
	}
}

func TestServerErrorBecomesBadGateway(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)

	_, err := svc.Post(context.Background(), 1)
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502 apperror", err)
	}
}

// TestMalformedUpstreamPayloadIsRejected matters because an entry cached once may
// be served for a long time; garbage must never get that far.
func TestMalformedUpstreamPayloadIsRejected(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>not json at all</html>`))
	}, nil)

	if _, err := svc.Post(context.Background(), 1); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestInvalidResourceIsRejected(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		// Well-formed JSON, nonsensical content.
		_, _ = w.Write([]byte(`{"userId":0,"id":0,"title":"","body":""}`))
	}, nil)

	if _, err := svc.Post(context.Background(), 1); err == nil {
		t.Fatal("expected validation to reject a degraded payload")
	}
}

func TestCollectionsDecodeAndForwardFilters(t *testing.T) {
	t.Parallel()
	var seenQuery string
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[{"userId":1,"id":1,"title":"a","completed":true}]`))
	}, nil)

	todos, err := svc.Todos(context.Background(), url.Values{"userId": {"1"}})
	if err != nil {
		t.Fatalf("Todos: %v", err)
	}
	if len(todos) != 1 || todos[0].ID != 1 {
		t.Fatalf("todos = %+v", todos)
	}
	if seenQuery != "userId=1" {
		t.Errorf("upstream saw query %q, want userId=1", seenQuery)
	}
}

// TestEmptyCollectionIsASuccess: the upstream returns `200 []` for an unmatched
// filter. That is an answer, not an absence, and it must marshal back as [] and
// never as null.
func TestEmptyCollectionIsASuccess(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}, nil)

	posts, err := svc.Posts(context.Background(), url.Values{"userId": {"99"}})
	if err != nil {
		t.Fatalf("Posts: %v", err)
	}
	if posts == nil {
		t.Fatal("empty result must be a non-nil slice so it encodes as [] rather than null")
	}
	if len(posts) != 0 {
		t.Fatalf("len = %d, want 0", len(posts))
	}
}

func TestBudgetExhaustionIsDistinctFromFailure(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"userId":1,"id":1,"title":"t","body":"b"}`))
	}, func(u *config.Upstream) {
		u.Budget.Enabled = true
		u.Budget.MaxCalls = 1
		u.Budget.Window = config.Duration(time.Hour)
	})

	if _, err := svc.Post(context.Background(), 1); err != nil {
		t.Fatalf("first call: %v", err)
	}

	_, err := svc.Post(context.Background(), 2)
	var appErr *apperror.Error
	if !errors.As(err, &appErr) || appErr.Code != apperror.CodeBudget {
		t.Fatalf("err = %v, want a budget-exhausted apperror distinct from an upstream failure", err)
	}
	if appErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", appErr.Status)
	}
}

func TestTodoIsDecoded(t *testing.T) {
	t.Parallel()
	svc := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"userId":1,"id":1,"title":"t","completed":true}`))
	}, nil)

	todo, err := svc.Todo(context.Background(), 1)
	if err != nil {
		t.Fatalf("Todo: %v", err)
	}
	if !todo.Completed {
		t.Error("completed should be true")
	}
}
