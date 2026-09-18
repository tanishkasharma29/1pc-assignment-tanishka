package upstream

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Budget is the global ceiling on billable upstream calls.
//
// It exists because a per-client rate limit does not bound cost: a thousand
// well-behaved clients can still spend without limit. This is the only mechanism
// in the system that is a true upper bound rather than a reduction.
//
// It tracks two independent limits and honours whichever is tighter:
//
//   - a local window budget we choose, and
//   - the upstream's own published allowance, read from the x-ratelimit-remaining
//     and x-ratelimit-reset headers present on every JSONPlaceholder response.
//
// Reading the upstream's own headers means the ceiling is grounded in what the
// provider actually permits rather than a number invented at design time.
type Budget struct {
	enabled bool
	max     int
	window  time.Duration
	reserve int

	mu        sync.Mutex
	used      int
	windowEnd time.Time

	// upstreamRemaining is -1 until a response teaches us otherwise.
	upstreamRemaining int
	upstreamReset     time.Time
}

// BudgetState is an immutable snapshot for logging and health reporting.
type BudgetState struct {
	Enabled           bool      `json:"enabled"`
	Used              int       `json:"used"`
	Max               int       `json:"max"`
	WindowEnds        time.Time `json:"window_ends"`
	UpstreamRemaining int       `json:"upstream_remaining"`
	UpstreamReset     time.Time `json:"upstream_reset,omitzero"`
}

func NewBudget(enabled bool, max int, window time.Duration, reserve int) *Budget {
	return &Budget{
		enabled:           enabled,
		max:               max,
		window:            window,
		reserve:           reserve,
		windowEnd:         time.Now().Add(window),
		upstreamRemaining: -1,
	}
}

// Allow reserves one call, returning false when the budget is spent. It is
// called per attempt rather than per request because a retry is just as
// billable as the original.
func (b *Budget) Allow() bool {
	if b == nil || !b.enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	if now.After(b.windowEnd) {
		b.used = 0
		b.windowEnd = now.Add(b.window)
	}
	// The upstream's own allowance, once known, overrides ours whenever it is
	// tighter. We stop short of exhausting it so unforeseen traffic is not left
	// with nothing.
	if b.upstreamRemaining >= 0 && now.Before(b.upstreamReset) && b.upstreamRemaining <= b.reserve {
		return false
	}
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

// Observe learns the upstream's published allowance from a response. Unparseable
// or absent headers are ignored: this is an optional tightening of the bound, so
// it must never make the system stricter by accident.
func (b *Budget) Observe(h http.Header) {
	if b == nil || !b.enabled || h == nil {
		return
	}
	remainingRaw := h.Get("X-RateLimit-Remaining")
	if remainingRaw == "" {
		return
	}
	remaining, err := strconv.Atoi(remainingRaw)
	if err != nil || remaining < 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.upstreamRemaining = remaining
	if resetRaw := h.Get("X-RateLimit-Reset"); resetRaw != "" {
		if epoch, err := strconv.ParseInt(resetRaw, 10, 64); err == nil {
			b.upstreamReset = time.Unix(epoch, 0)
		}
	}
}

func (b *Budget) Snapshot() BudgetState {
	if b == nil {
		return BudgetState{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return BudgetState{
		Enabled:           b.enabled,
		Used:              b.used,
		Max:               b.max,
		WindowEnds:        b.windowEnd,
		UpstreamRemaining: b.upstreamRemaining,
		UpstreamReset:     b.upstreamReset,
	}
}
