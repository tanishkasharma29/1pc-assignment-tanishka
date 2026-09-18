package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the shared, cross-instance cache tier.
//
// Its purpose is coordination, not capacity. The entire upstream dataset is
// roughly 51 KB and fits comfortably in any single process, so Redis earns its
// place only when several instances run behind a load balancer: without a shared
// store, N cold instances each make their own upstream call for the same key.
type Redis struct {
	client *redis.Client
	prefix string

	hits   atomic.Uint64
	misses atomic.Uint64
	sets   atomic.Uint64
	errs   atomic.Uint64
}

// RedisOptions configures the client. Timeouts are mandatory in spirit: an
// unbounded Redis call would turn a cache lookup, which exists to make things
// faster, into a source of latency.
type RedisOptions struct {
	Addr         string
	Password     string
	DB           int
	KeyPrefix    string
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

func NewRedis(opts RedisOptions) *Redis {
	return &Redis{
		client: redis.NewClient(&redis.Options{
			Addr:         opts.Addr,
			Password:     opts.Password,
			DB:           opts.DB,
			DialTimeout:  opts.DialTimeout,
			ReadTimeout:  opts.ReadTimeout,
			WriteTimeout: opts.WriteTimeout,
			PoolSize:     opts.PoolSize,
		}),
		prefix: opts.KeyPrefix,
	}
}

// NewRedisWithClient allows tests to inject a client pointed at miniredis.
func NewRedisWithClient(client *redis.Client, prefix string) *Redis {
	return &Redis{client: client, prefix: prefix}
}

func (r *Redis) Name() string { return "redis" }

func (r *Redis) Client() *redis.Client { return r.client }

func (r *Redis) key(k string) string { return r.prefix + "entry:" + k }

func (r *Redis) Get(ctx context.Context, key string) (*Entry, bool, error) {
	raw, err := r.client.Get(ctx, r.key(key)).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		r.misses.Add(1)
		return nil, false, nil
	case err != nil:
		r.errs.Add(1)
		r.misses.Add(1)
		// Reported as a miss, not a failure: a degraded cache must cost money,
		// never availability.
		return nil, false, fmt.Errorf("redis get: %w", err)
	}

	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		r.errs.Add(1)
		r.misses.Add(1)
		// A corrupt entry is evicted rather than returned; the next request
		// repopulates it correctly.
		_ = r.client.Del(ctx, r.key(key)).Err()
		return nil, false, fmt.Errorf("redis decode entry: %w", err)
	}

	// Redis expires keys itself, but an entry written with ttl 0 lives forever,
	// so the embedded deadline is still authoritative.
	if entry.Expired(time.Now()) {
		r.misses.Add(1)
		_ = r.client.Del(ctx, r.key(key)).Err()
		return nil, false, nil
	}

	r.hits.Add(1)
	return &entry, true, nil
}

// Set writes synchronously and waits for the server's acknowledgement.
//
// This is load-bearing. The caching middleware calls Set before releasing the
// requests waiting on the same key, so a fire-and-forget or pipelined write
// would let those requests observe an empty cache and issue duplicate upstream
// calls — exactly the race the middleware exists to prevent.
func (r *Redis) Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	stored := entry.Clone()
	now := time.Now()
	stored.StoredAt = now
	if ttl > 0 {
		stored.ExpiresAt = now.Add(ttl)
	} else {
		stored.ExpiresAt = time.Time{}
	}

	raw, err := json.Marshal(stored)
	if err != nil {
		r.errs.Add(1)
		return fmt.Errorf("redis encode entry: %w", err)
	}
	// A ttl of 0 is passed through as "no expiration", which is what immutable
	// resources want.
	if err := r.client.Set(ctx, r.key(key), raw, ttl).Err(); err != nil {
		r.errs.Add(1)
		return fmt.Errorf("redis set: %w", err)
	}
	r.sets.Add(1)
	return nil
}

func (r *Redis) Delete(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.key(key)).Err(); err != nil {
		r.errs.Add(1)
		return fmt.Errorf("redis del: %w", err)
	}
	return nil
}

func (r *Redis) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}

func (r *Redis) Stats() Stats {
	return Stats{
		Name:   r.Name(),
		Hits:   r.hits.Load(),
		Misses: r.misses.Load(),
		Sets:   r.sets.Load(),
		Errors: r.errs.Load(),
	}
}

func (r *Redis) Close() error { return r.client.Close() }
