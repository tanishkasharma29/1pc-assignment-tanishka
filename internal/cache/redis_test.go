package cache

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// miniredis keeps these tests hermetic: the Redis paths are exercised in CI with
// no live server and no network.
func newTestRedis(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedisWithClient(client, "test:"), server
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestRedisRoundTrip(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestRedis(t)

	if _, ok, _ := c.Get(ctx, "absent"); ok {
		t.Fatal("empty cache reported a hit")
	}
	if err := c.Set(ctx, "k", entry(200, `{"a":1}`), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Status != 200 || string(got.Body) != `{"a":1}` {
		t.Fatalf("round-trip mismatch: status=%d body=%q", got.Status, got.Body)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Errorf("headers did not survive serialisation: %v", got.Header)
	}
}

func TestRedisTTLExpiry(t *testing.T) {
	ctx := context.Background()
	c, server := newTestRedis(t)

	_ = c.Set(ctx, "expiring", entry(200, "x"), time.Minute)
	server.FastForward(2 * time.Minute)

	if _, ok, _ := c.Get(ctx, "expiring"); ok {
		t.Error("entry outlived its ttl")
	}
}

// TestRedisZeroTTLNeverExpires: immutable resources are stored without a
// deadline, which Redis expresses as no expiration at all.
func TestRedisZeroTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	c, server := newTestRedis(t)

	_ = c.Set(ctx, "immortal", entry(200, "x"), 0)
	server.FastForward(72 * time.Hour)

	if _, ok, _ := c.Get(ctx, "immortal"); !ok {
		t.Error("an entry stored with ttl 0 should never expire")
	}
}

// TestRedisCorruptEntryIsEvicted: a bad entry is discarded rather than returned,
// so the next request repopulates it correctly instead of serving garbage.
func TestRedisCorruptEntryIsEvicted(t *testing.T) {
	ctx := context.Background()
	c, server := newTestRedis(t)

	if err := server.Set("test:entry:broken", "this is not json"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Get(ctx, "broken"); ok || err == nil {
		t.Fatalf("corrupt entry: ok=%v err=%v; want a reported miss", ok, err)
	}
	if server.Exists("test:entry:broken") {
		t.Error("corrupt entry should have been deleted")
	}
}

func TestRedisUnreachableIsAMissNotAFailure(t *testing.T) {
	ctx := context.Background()
	c, server := newTestRedis(t)
	server.Close() // simulate an outage

	_, ok, err := c.Get(ctx, "k")
	if ok {
		t.Error("expected a miss when the backend is down")
	}
	if err == nil {
		t.Error("the error should still be reported so it can be logged and counted")
	}
}

// TestTieredPromotesFromSharedTier is the behaviour that makes Redis a
// coordination layer: a second instance fills its local cache from the shared
// tier rather than from a second upstream call.
func TestTieredPromotesFromSharedTier(t *testing.T) {
	ctx := context.Background()
	shared, _ := newTestRedis(t)

	local := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = local.Close() })
	tiered := NewTiered(local, shared, quietLogger())

	// Another instance stored this.
	if err := shared.Set(ctx, "k", entry(200, `{"from":"peer"}`), time.Minute); err != nil {
		t.Fatal(err)
	}

	got, ok, err := tiered.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if string(got.Body) != `{"from":"peer"}` {
		t.Fatalf("body = %q", got.Body)
	}

	// The promotion means the next read is local and costs no network hop.
	if _, ok, _ := local.Get(ctx, "k"); !ok {
		t.Error("an entry found in the shared tier should be promoted into the local one")
	}
}

func TestTieredWritesBothTiers(t *testing.T) {
	ctx := context.Background()
	shared, _ := newTestRedis(t)
	local := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = local.Close() })
	tiered := NewTiered(local, shared, quietLogger())

	if err := tiered.Set(ctx, "k", entry(200, "v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok, _ := local.Get(ctx, "k"); !ok {
		t.Error("local tier was not written")
	}
	if _, ok, _ := shared.Get(ctx, "k"); !ok {
		t.Error("shared tier was not written")
	}
}

// TestTieredSurvivesSharedTierOutage: losing Redis must cost money, not
// availability, and must not also disable the local cache.
func TestTieredSurvivesSharedTierOutage(t *testing.T) {
	ctx := context.Background()
	shared, server := newTestRedis(t)
	local := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = local.Close() })
	tiered := NewTiered(local, shared, quietLogger())

	server.Close()

	// Set reports the shared-tier failure but must still populate the local one.
	_ = tiered.Set(ctx, "k", entry(200, "v"), time.Minute)
	if _, ok, _ := local.Get(ctx, "k"); !ok {
		t.Error("a redis outage must not disable local caching as well")
	}
	if _, ok, err := tiered.Get(ctx, "k"); !ok || err != nil {
		t.Errorf("reads should still be served locally during an outage: ok=%v err=%v", ok, err)
	}
}
