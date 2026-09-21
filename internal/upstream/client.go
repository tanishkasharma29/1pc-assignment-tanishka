// Package upstream is the HTTP client for the billable third-party API.
//
// It is deliberately ignorant of application semantics: a 404 is returned as a
// Response, not an error, because whether a 404 is a failure or a valid cacheable
// answer is a decision for the service layer. The client only reports errors for
// things that genuinely prevented a response from being obtained.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/config"
)

// ErrBudgetExhausted is returned when the global budget refuses a call. It is
// distinct from an upstream failure: nothing went wrong, we simply declined to
// spend.
var ErrBudgetExhausted = errors.New("upstream call budget exhausted")

// errAttemptTimeout marks a failure caused by one attempt's own deadline rather
// than by the caller giving up.
//
// The distinction cannot be recovered from the error alone: an attempt context
// is derived from the caller's, so both expiries surface as
// context.DeadlineExceeded. Only do, which holds both, can tell them apart —
// and it must, because they call for opposite responses. An attempt that ran
// out of time is the transient slowness retries exist for; a caller that gave
// up means nobody is waiting, and further attempts would only spend budget.
var errAttemptTimeout = errors.New("upstream attempt deadline exceeded")

// Response is a completed upstream exchange. A non-2xx Status is a normal
// outcome, not an error.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Client fetches from the upstream API.
type Client struct {
	base   *url.URL
	hc     *http.Client
	cfg    config.Upstream
	log    *slog.Logger
	budget *Budget

	// calls counts billable attempts actually dispatched. This is the number the
	// whole system exists to minimise, so it is measured rather than assumed.
	calls atomic.Int64
}

func New(cfg config.Upstream, log *slog.Logger) (*Client, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream base url %q: %w", cfg.BaseURL, err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = cfg.MaxIdleConns
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.ForceAttemptHTTP2 = true

	return &Client{
		base: base,
		// No timeout on the client itself: each attempt gets its own context
		// deadline, so a per-client timeout would silently cap the retry
		// sequence as a whole.
		hc:  &http.Client{Transport: transport},
		cfg: cfg,
		log: log,
		budget: NewBudget(cfg.Budget.Enabled, cfg.Budget.MaxCalls,
			time.Duration(cfg.Budget.Window), cfg.Budget.ReserveRemaining),
	}, nil
}

// Calls reports how many billable attempts have been dispatched.
func (c *Client) Calls() int64 { return c.calls.Load() }

func (c *Client) Budget() *Budget { return c.budget }

// Get performs a GET with bounded retries.
//
// Retries cover transient conditions only — network errors, 5xx and 429. A 4xx
// is a deterministic answer about an immutable resource; retrying it would spend
// money to receive the same reply.
func (c *Client) Get(ctx context.Context, path string, query url.Values) (*Response, error) {
	target := *c.base
	target.Path = strings.TrimSuffix(c.base.Path, "/") + path
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	var lastErr error
	for attempt := 1; attempt <= c.cfg.MaxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
				return nil, apperror.UpstreamTimeout(err)
			}
		}

		if !c.budget.Allow() {
			c.log.WarnContext(ctx, "upstream call refused by budget",
				slog.String("url", target.String()),
				slog.Any("budget", c.budget.Snapshot()))
			return nil, ErrBudgetExhausted
		}

		resp, err := c.do(ctx, target.String())
		if err != nil {
			lastErr = err
			// A cancelled or expired caller context is terminal: retrying would
			// spend budget on a request nobody is waiting for any more.
			if ctx.Err() != nil {
				break
			}
			if !retryableErr(err) {
				break
			}
			c.log.WarnContext(ctx, "upstream attempt failed, retrying",
				slog.String("url", target.String()),
				slog.Int("attempt", attempt),
				slog.String("error", err.Error()))
			continue
		}

		if retryableStatus(resp.Status) && attempt < c.cfg.MaxAttempts {
			lastErr = fmt.Errorf("upstream returned %d", resp.Status)
			c.log.WarnContext(ctx, "upstream returned retryable status",
				slog.String("url", target.String()),
				slog.Int("status", resp.Status),
				slog.Int("attempt", attempt))
			continue
		}
		return resp, nil
	}

	if lastErr == nil {
		lastErr = errors.New("upstream request failed")
	}
	if isTimeout(lastErr) || errors.Is(lastErr, context.DeadlineExceeded) {
		return nil, apperror.UpstreamTimeout(lastErr)
	}
	return nil, apperror.Upstream(lastErr)
}

// do dispatches a single attempt under its own deadline.
func (c *Client) do(ctx context.Context, rawURL string) (*Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(c.cfg.Timeout))
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "typecode-caching-proxy/1.0")

	started := time.Now()
	c.calls.Add(1) // counted here: the request is now on the wire and billable

	resp, err := c.hc.Do(req)
	if err != nil {

		// Attribute the deadline here, where both contexts are still in scope.
		// An expired attempt under a live caller is retryable; an expired caller
		// is not. Past this point the two are indistinguishable.
		if attemptCtx.Err() != nil && ctx.Err() == nil {
            return nil, fmt.Errorf("upstream request: %w: %w", errAttemptTimeout, err)
        }
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	// LimitReader bounds memory even if the upstream misreports Content-Length.
	// The extra byte lets us detect truncation rather than silently caching a
	// partial body forever.
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream body: %w", err)
	}
	if int64(len(body)) > c.cfg.MaxResponseBytes {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", c.cfg.MaxResponseBytes)
	}

	c.budget.Observe(resp.Header)
	c.log.DebugContext(ctx, "upstream call completed",
		slog.String("url", rawURL),
		slog.Int("status", resp.StatusCode),
		slog.Int("bytes", len(body)),
		slog.Duration("duration", time.Since(started)))

	return &Response{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: body}, nil
}

// backoff returns exponential delay with full jitter. Jitter matters here: it
// prevents a fleet of instances that failed together from retrying together.
func (c *Client) backoff(attempt int) time.Duration {
	base := time.Duration(c.cfg.RetryBaseDelay)
	maxDelay := time.Duration(c.cfg.RetryMaxDelay)
	d := base << (attempt - 2) // attempt 2 is the first retry
	if d <= 0 || d > maxDelay {
		d = maxDelay
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryableStatus covers transient server-side conditions only.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func retryableErr(err error) bool {
 	// This attempt's own deadline fired while the caller was still waiting —
	// precisely the transient slowness retries exist for. Checked first, because
	// the error also carries context.DeadlineExceeded, which the clause below
	// would otherwise read as the caller having given up.
	if errors.Is(err, errAttemptTimeout) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return true // connection resets and EOF are transient and worth one more try
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
