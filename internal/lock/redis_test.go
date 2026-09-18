package lock

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestLocker(t *testing.T) (*RedisLocker, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewRedis(client, "test:", 10*time.Second, log), server
}

func TestLockIsExclusive(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t)

	release, acquired, err := locker.Acquire(ctx, "key")
	if err != nil || !acquired {
		t.Fatalf("first acquire: acquired=%v err=%v", acquired, err)
	}

	if _, acquired, err := locker.Acquire(ctx, "key"); err != nil || acquired {
		t.Fatalf("second acquire: acquired=%v err=%v; want it to be refused", acquired, err)
	}

	release(ctx)

	if _, acquired, err := locker.Acquire(ctx, "key"); err != nil || !acquired {
		t.Fatalf("after release: acquired=%v err=%v", acquired, err)
	}
}

func TestDifferentKeysDoNotContend(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t)

	if _, ok, _ := locker.Acquire(ctx, "a"); !ok {
		t.Fatal("could not acquire a")
	}
	if _, ok, _ := locker.Acquire(ctx, "b"); !ok {
		t.Error("distinct keys must not contend")
	}
}

// TestLockExpires bounds the damage from a holder that crashes: the lock lapses
// on its own rather than wedging the fleet until someone intervenes.
func TestLockExpires(t *testing.T) {
	ctx := context.Background()
	locker, server := newTestLocker(t)

	if _, ok, _ := locker.Acquire(ctx, "key"); !ok {
		t.Fatal("could not acquire")
	}
	server.FastForward(11 * time.Second)

	if _, ok, err := locker.Acquire(ctx, "key"); err != nil || !ok {
		t.Fatalf("after ttl: acquired=%v err=%v; an abandoned lock must expire", ok, err)
	}
}

// TestReleaseOnlyDeletesOwnLock is why release is a compare-and-delete rather
// than a plain DEL. A leader that overran its TTL must not delete the lock a
// different instance has since taken, or two instances would fetch at once.
func TestReleaseOnlyDeletesOwnLock(t *testing.T) {
	ctx := context.Background()
	locker, server := newTestLocker(t)

	staleRelease, ok, _ := locker.Acquire(ctx, "key")
	if !ok {
		t.Fatal("could not acquire")
	}

	server.FastForward(11 * time.Second) // the first holder overran its ttl

	if _, ok, _ := locker.Acquire(ctx, "key"); !ok {
		t.Fatal("a second holder should now be able to acquire")
	}

	staleRelease(ctx) // the overrun holder belatedly releases

	if _, ok, _ := locker.Acquire(ctx, "key"); ok {
		t.Error("the stale release deleted the new holder's lock; release must compare tokens")
	}
}

func TestUnreachableRedisFailsOpen(t *testing.T) {
	ctx := context.Background()
	locker, server := newTestLocker(t)
	server.Close()

	_, acquired, err := locker.Acquire(ctx, "key")
	if err == nil {
		t.Error("an unreachable lock backend should report an error so it can be logged")
	}
	if acquired {
		t.Error("a failed acquire must not claim the lock")
	}
	// The caller treats this as "proceed anyway": a lock we cannot reach may cost
	// a duplicate upstream call, but blocking on it would cost availability.
}

func TestNoOpLockerAlwaysGrants(t *testing.T) {
	ctx := context.Background()
	var locker Locker = NoOp{}

	for range 5 {
		release, acquired, err := locker.Acquire(ctx, "key")
		if err != nil || !acquired {
			t.Fatalf("NoOp must always grant: acquired=%v err=%v", acquired, err)
		}
		release(ctx)
	}
}
