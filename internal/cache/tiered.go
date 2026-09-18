package cache

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Tiered composes a fast local tier (L1, in-process) with a shared tier
// (L2, Redis).
//
// The division of labour is the point of the design:
//
//   - L1 is the capacity layer. It holds the whole dataset and serves every hit
//     without a network round trip. On a single instance it is sufficient alone.
//   - L2 is the coordination layer. It lets instance B answer from instance A's
//     upstream call instead of making its own, which is the only reason a 51 KB
//     dataset needs a network cache at all.
type Tiered struct {
	l1  Cache
	l2  Cache
	log *slog.Logger

	hits     atomic.Uint64
	misses   atomic.Uint64
	promoted atomic.Uint64
	l2Errors atomic.Uint64
}

func NewTiered(l1, l2 Cache, log *slog.Logger) *Tiered {
	return &Tiered{l1: l1, l2: l2, log: log}
}

func (t *Tiered) Name() string { return t.l1.Name() + "+" + t.l2.Name() }

// Get consults L1 first, then L2. An L2 hit is promoted into L1 so that repeat
// reads on this instance stop crossing the network.
func (t *Tiered) Get(ctx context.Context, key string) (*Entry, bool, error) {
	if entry, ok, _ := t.l1.Get(ctx, key); ok {
		t.hits.Add(1)
		return entry, true, nil
	}

	entry, ok, err := t.l2.Get(ctx, key)
	if err != nil {
		t.l2Errors.Add(1)
		// Downgraded to a warning: L2 being unreachable costs upstream calls,
		// not correctness, so the request proceeds as a miss.
		t.log.WarnContext(ctx, "shared cache read failed; treating as miss",
			slog.String("key", key), slog.String("error", err.Error()))
	}
	if !ok {
		t.misses.Add(1)
		return nil, false, nil
	}

	t.promoted.Add(1)
	// Promote with the remaining lifetime, not the original TTL, so an entry
	// cannot be kept alive indefinitely by being repeatedly copied down a tier.
	if err := t.l1.Set(ctx, key, entry, remainingTTL(entry)); err != nil {
		t.log.WarnContext(ctx, "local cache promotion failed",
			slog.String("key", key), slog.String("error", err.Error()))
	}
	t.hits.Add(1)
	return entry, true, nil
}

// Set writes both tiers and returns only once both have acknowledged.
//
// L2 is written first so that by the time this returns, the entry is visible to
// every instance, not just this one. Both writes are awaited because the caching
// middleware releases waiting requests immediately after Set returns.
func (t *Tiered) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error {
	l2Err := t.l2.Set(ctx, key, entry, ttl)
	if l2Err != nil {
		t.l2Errors.Add(1)
		t.log.WarnContext(ctx, "shared cache write failed; entry is local only",
			slog.String("key", key), slog.String("error", l2Err.Error()))
	}
	// L1 is written regardless: losing the shared tier must not also lose the
	// local one, or a Redis outage would remove all caching at once.
	if err := t.l1.Set(ctx, key, entry, ttl); err != nil {
		return err
	}
	return l2Err
}

func (t *Tiered) Delete(ctx context.Context, key string) error {
	err1 := t.l1.Delete(ctx, key)
	if err := t.l2.Delete(ctx, key); err != nil {
		return err
	}
	return err1
}

// Ping reports L2 reachability; L1 cannot be unreachable.
func (t *Tiered) Ping(ctx context.Context) error { return t.l2.Ping(ctx) }

func (t *Tiered) Stats() Stats {
	local := t.l1.Stats()
	shared := t.l2.Stats()
	return Stats{
		Name:            t.Name(),
		Hits:            t.hits.Load(),
		Misses:          t.misses.Load(),
		Sets:            local.Sets,
		Evictions:       local.Evictions,
		Expirations:     local.Expirations,
		Rejected:        local.Rejected,
		Errors:          t.l2Errors.Load() + shared.Errors,
		Entries:         local.Entries,
		NegativeEntries: local.NegativeEntries,
		Bytes:           local.Bytes,
	}
}

func (t *Tiered) Close() error {
	err := t.l1.Close()
	if err2 := t.l2.Close(); err2 != nil {
		return err2
	}
	return err
}

// remainingTTL converts an absolute deadline back into a duration. A zero
// deadline means the entry never expires and must stay that way.
func remainingTTL(e *Entry) time.Duration {
	if e.ExpiresAt.IsZero() {
		return 0
	}
	if d := time.Until(e.ExpiresAt); d > 0 {
		return d
	}
	return time.Millisecond // already expiring; let it lapse naturally
}
