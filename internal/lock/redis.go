package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// releaseScript deletes the lock only if we still hold it.
//
// The comparison is essential. Without it, a leader that overran its TTL would
// delete a lock a different instance had since acquired, letting two instances
// fetch concurrently — reintroducing the duplicate call this package prevents.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

// RedisLocker implements Locker with SET NX PX.
type RedisLocker struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
	log    *slog.Logger
}

func NewRedis(client *redis.Client, prefix string, ttl time.Duration, log *slog.Logger) *RedisLocker {
	return &RedisLocker{client: client, prefix: prefix, ttl: ttl, log: log}
}

func (l *RedisLocker) Name() string { return "redis" }

func (l *RedisLocker) Acquire(ctx context.Context, key string) (Release, bool, error) {
	token, err := newToken()
	if err != nil {
		return nil, false, fmt.Errorf("generate lock token: %w", err)
	}
	fullKey := l.prefix + "lock:" + key

	// The TTL is what bounds damage from a crashed holder: the lock lapses on
	// its own rather than wedging the fleet until someone intervenes.
	ok, err := l.client.SetNX(ctx, fullKey, token, l.ttl).Result()
	if err != nil {
		// Fail open. A lock we cannot reach must not stop us serving; the worst
		// case is a duplicate upstream call, and the budget still caps spend.
		return nil, false, fmt.Errorf("acquire lock: %w", err)
	}
	if !ok {
		return nil, false, nil
	}

	release := func(releaseCtx context.Context) {
		if err := releaseScript.Run(releaseCtx, l.client, []string{fullKey}, token).Err(); err != nil {
			// Not fatal: the TTL guarantees the lock is eventually freed.
			l.log.WarnContext(releaseCtx, "failed to release cache lock; relying on ttl",
				slog.String("key", key), slog.String("error", err.Error()))
		}
	}
	return release, true, nil
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
