package cache

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func entry(status int, body string) *Entry {
	return &Entry{
		Status: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(body),
	}
}

func TestMemoryGetSet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = c.Close() })

	if _, ok, _ := c.Get(ctx, "absent"); ok {
		t.Fatal("empty cache reported a hit")
	}
	if err := c.Set(ctx, "k", entry(200, `{"a":1}`), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get after Set: ok=%v err=%v", ok, err)
	}
	if string(got.Body) != `{"a":1}` || got.Status != 200 {
		t.Fatalf("round-trip mismatch: status=%d body=%q", got.Status, got.Body)
	}
}

// TestMemoryReturnsCopies guards against a handler mutating a shared entry and
// corrupting every response served from it afterwards.
func TestMemoryReturnsCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4})
	t.Cleanup(func() { _ = c.Close() })

	_ = c.Set(ctx, "k", entry(200, `original`), 0)

	first, _, _ := c.Get(ctx, "k")
	first.Body[0] = 'X'
	first.Header.Set("Content-Type", "text/plain")

	second, _, _ := c.Get(ctx, "k")
	if string(second.Body) != "original" {
		t.Errorf("body = %q; a caller mutated the stored entry", second.Body)
	}
	if second.Header.Get("Content-Type") != "application/json" {
		t.Errorf("header was mutated through a returned copy")
	}
}

// TestMemoryTTL uses an injected clock so expiry is asserted deterministically
// rather than by sleeping.
func TestMemoryTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	now := time.Now()
	c := NewMemory(MemoryOptions{
		MaxEntries: 8, MaxNegativeEntries: 4,
		Now: func() time.Time { return now },
	})
	t.Cleanup(func() { _ = c.Close() })

	_ = c.Set(ctx, "expiring", entry(200, "x"), time.Minute)
	_ = c.Set(ctx, "immortal", entry(200, "y"), 0) // 0 means never expires

	now = now.Add(59 * time.Second)
	if _, ok, _ := c.Get(ctx, "expiring"); !ok {
		t.Error("entry expired before its ttl elapsed")
	}

	now = now.Add(2 * time.Second)
	if _, ok, _ := c.Get(ctx, "expiring"); ok {
		t.Error("entry survived past its ttl")
	}
	if _, ok, _ := c.Get(ctx, "immortal"); !ok {
		t.Error("ttl of 0 must mean the entry never expires, which is correct for immutable data")
	}
}

func TestMemoryLRUEviction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 3, MaxNegativeEntries: 2})
	t.Cleanup(func() { _ = c.Close() })

	for _, k := range []string{"a", "b", "c"} {
		_ = c.Set(ctx, k, entry(200, k), 0)
	}
	// Touch "a" so "b" becomes the least recently used.
	if _, ok, _ := c.Get(ctx, "a"); !ok {
		t.Fatal("a should still be present")
	}
	_ = c.Set(ctx, "d", entry(200, "d"), 0)

	if _, ok, _ := c.Get(ctx, "b"); ok {
		t.Error("b was least recently used and should have been evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok, _ := c.Get(ctx, k); !ok {
			t.Errorf("%s should have been retained", k)
		}
	}
}

// TestNegativePartitionDoesNotEvictPositiveEntries is the containment property
// the two-partition design exists for.
//
// A caller can request unlimited nonexistent ids, so 404 keys are unbounded in
// number and chosen by them, not us. Sharing a single LRU would let a trivial
// loop flush the real dataset out of the cache — turning every subsequent
// legitimate request back into a billable upstream call.
func TestNegativePartitionDoesNotEvictPositiveEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 10, MaxNegativeEntries: 5})
	t.Cleanup(func() { _ = c.Close() })

	for i := range 10 {
		_ = c.Set(ctx, fmt.Sprintf("real-%d", i), entry(200, "payload"), 0)
	}

	// Simulate an attacker walking nonexistent ids.
	for i := range 500 {
		_ = c.Set(ctx, fmt.Sprintf("missing-%d", i), entry(404, "{}"), time.Minute)
	}

	for i := range 10 {
		if _, ok, _ := c.Get(ctx, fmt.Sprintf("real-%d", i)); !ok {
			t.Fatalf("real-%d was evicted by 404 traffic; the negative partition is not isolated", i)
		}
	}

	stats := c.Stats()
	if stats.NegativeEntries > 5 {
		t.Errorf("negative partition holds %d entries, want at most 5", stats.NegativeEntries)
	}
}

// TestStatusDecidesPartitionNotEmptiness: the upstream answers an unmatched
// filter with `200 []`, which is a successful, cacheable result.
func TestEmptySuccessIsAPositiveEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 4, MaxNegativeEntries: 2})
	t.Cleanup(func() { _ = c.Close() })

	_ = c.Set(ctx, "empty", entry(200, "[]"), 0)

	stats := c.Stats()
	if stats.Entries != 1 || stats.NegativeEntries != 0 {
		t.Errorf("entries=%d negative=%d; an empty 200 belongs in the positive partition",
			stats.Entries, stats.NegativeEntries)
	}
}

func TestMemoryRejectsEntryLargerThanTotalBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 8, MaxNegativeEntries: 4, MaxBytes: 128})
	t.Cleanup(func() { _ = c.Close() })

	_ = c.Set(ctx, "small", entry(200, "ok"), 0)
	_ = c.Set(ctx, "huge", entry(200, string(make([]byte, 4096))), 0)

	if _, ok, _ := c.Get(ctx, "huge"); ok {
		t.Error("an entry larger than the whole budget should be refused, not admitted")
	}
	if _, ok, _ := c.Get(ctx, "small"); !ok {
		t.Error("admitting an oversized entry must not evict everything else")
	}
}

func TestMemoryByteBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 1000, MaxNegativeEntries: 100, MaxBytes: 4096})
	t.Cleanup(func() { _ = c.Close() })

	for i := range 200 {
		_ = c.Set(ctx, fmt.Sprintf("k%d", i), entry(200, string(make([]byte, 256))), 0)
	}

	if got := c.Stats().Bytes; got > 4096 {
		t.Errorf("cache holds %d bytes, exceeding the 4096 budget", got)
	}
}

// TestMemoryConcurrentAccess is run under -race in CI; it exists to catch
// locking mistakes rather than to assert a specific outcome.
func TestMemoryConcurrentAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := NewMemory(MemoryOptions{MaxEntries: 64, MaxNegativeEntries: 16})
	t.Cleanup(func() { _ = c.Close() })

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(3)
		go func() { defer wg.Done(); _ = c.Set(ctx, fmt.Sprintf("k%d", i%10), entry(200, "v"), time.Minute) }()
		go func() { defer wg.Done(); _, _, _ = c.Get(ctx, fmt.Sprintf("k%d", i%10)) }()
		go func() { defer wg.Done(); _ = c.Delete(ctx, fmt.Sprintf("k%d", i%10)) }()
	}
	wg.Wait()

	if got := c.Stats().Bytes; got < 0 {
		t.Errorf("byte accounting went negative: %d", got)
	}
}

func TestStatsHitRatio(t *testing.T) {
	t.Parallel()
	if got := (Stats{}).HitRatio(); got != 0 {
		t.Errorf("empty stats hit ratio = %v, want 0", got)
	}
	if got := (Stats{Hits: 3, Misses: 1}).HitRatio(); got != 0.75 {
		t.Errorf("hit ratio = %v, want 0.75", got)
	}
}
