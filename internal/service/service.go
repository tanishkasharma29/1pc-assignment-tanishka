// Package service holds the business logic between the HTTP transport and the
// upstream client.
//
// Its main job is translating upstream outcomes into application meaning: a 404
// from the upstream is a legitimate, cacheable answer about immutable data,
// whereas a 503 is a failure that must never be stored. Keeping that judgement
// here means the upstream client stays a dumb transport and the transport layer
// stays free of upstream quirks.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/model"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

// validatable is satisfied by every upstream resource. Requiring it means a
// decoded payload is always checked before it can be cached, so a degraded
// upstream response cannot be stored and then served for the lifetime of an
// immutable entry.
type validatable interface {
	Validate() error
}

type Service struct {
	up  *upstream.Client
	log *slog.Logger
}

func New(up *upstream.Client, log *slog.Logger) *Service {
	return &Service{up: up, log: log}
}

func (s *Service) Post(ctx context.Context, id int) (model.Post, error) {
	return fetchOne[model.Post](ctx, s, "/posts/"+strconv.Itoa(id), "post not found")
}

func (s *Service) Todo(ctx context.Context, id int) (model.Todo, error) {
	return fetchOne[model.Todo](ctx, s, "/todos/"+strconv.Itoa(id), "todo not found")
}

func (s *Service) Posts(ctx context.Context, query url.Values) ([]model.Post, error) {
	return fetchMany[model.Post](ctx, s, "/posts", query)
}

func (s *Service) Todos(ctx context.Context, query url.Values) ([]model.Todo, error) {
	return fetchMany[model.Todo](ctx, s, "/todos", query)
}

func fetchOne[T validatable](ctx context.Context, s *Service, path, notFound string) (T, error) {
	var zero T

	resp, err := s.fetch(ctx, path, nil)
	if err != nil {
		return zero, err
	}
	if resp.Status == http.StatusNotFound {
		// Not an error condition to retry or alarm on: for immutable data this
		// is a permanent, correct answer, and caching it is what stops repeated
		// lookups of a missing id from billing us every time.
		return zero, apperror.NotFound(notFound)
	}
	if err := ensureOK(resp, path); err != nil {
		return zero, err
	}

	var out T
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return zero, apperror.Upstream(fmt.Errorf("decode %s: %w", path, err))
	}
	if err := out.Validate(); err != nil {
		return zero, apperror.Upstream(fmt.Errorf("validate %s: %w", path, err))
	}
	return out, nil
}

func fetchMany[T validatable](ctx context.Context, s *Service, path string, query url.Values) ([]T, error) {
	resp, err := s.fetch(ctx, path, query)
	if err != nil {
		return nil, err
	}
	if err := ensureOK(resp, path); err != nil {
		return nil, err
	}

	var out []T
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, apperror.Upstream(fmt.Errorf("decode %s: %w", path, err))
	}
	for i, item := range out {
		if err := item.Validate(); err != nil {
			return nil, apperror.Upstream(fmt.Errorf("validate %s[%d]: %w", path, i, err))
		}
	}
	// A nil slice would marshal to null; an empty collection must be []. The
	// upstream genuinely returns an empty array for an unmatched filter, and
	// that is a successful answer worth caching, not an absence.
	if out == nil {
		out = []T{}
	}
	return out, nil
}

func (s *Service) fetch(ctx context.Context, path string, query url.Values) (*upstream.Response, error) {
	resp, err := s.up.Get(ctx, path, query)
	if err != nil {
		if errors.Is(err, upstream.ErrBudgetExhausted) {
			// Distinguished from an upstream failure: nothing broke, we declined
			// to spend. Surfacing it as its own status makes that visible.
			s.log.WarnContext(ctx, "request refused to protect upstream budget",
				slog.String("path", path))
			return nil, apperror.BudgetExhausted()
		}
		return nil, err
	}
	return resp, nil
}

func ensureOK(resp *upstream.Response, path string) error {
	if resp.Status >= http.StatusOK && resp.Status < http.StatusMultipleChoices {
		return nil
	}
	return apperror.Upstream(fmt.Errorf("upstream %s returned status %d", path, resp.Status))
}
