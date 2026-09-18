// Package cache stores complete HTTP responses.
//
// The unit of storage is a whole response — status, allowlisted headers and body
// — because the caching middleware replays responses verbatim rather than
// re-rendering them. Entries are immutable once stored; there is no update path,
// only expiry, which matches the immutability guarantee of the upstream data.
package cache

import (
	"context"
	"net/http"
	"time"
)

// Entry is a cached HTTP response.
//
// Body is []byte, which encoding/json renders as base64 — slightly larger on the
// wire to Redis, but it round-trips any payload without corruption, which a
// string field would not guarantee.
type Entry struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body"`
	// StoredAt supports the X-Cache-Age response header and makes stale entries
	// identifiable in logs.
	StoredAt time.Time `json:"stored_at"`
	// ExpiresAt zero means "never expires", which is the correct policy for
	// immutable resources.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Positive reports whether this entry represents a successful response. It
// decides which LRU partition the entry occupies, and it is deliberately based
// on status alone: an empty collection (`200 []`, which the upstream returns for
// ?userId=99) is a success, not a negative result.
func (e *Entry) Positive() bool {
	return e.Status >= 200 && e.Status < 300
}

func (e *Entry) Expired(now time.Time) bool {
	return !e.ExpiresAt.IsZero() && now.After(e.ExpiresAt)
}

func (e *Entry) Age(now time.Time) time.Duration {
	if e.StoredAt.IsZero() {
		return 0
	}
	if d := now.Sub(e.StoredAt); d > 0 {
		return d
	}
	return 0
}

// Size approximates the entry's memory footprint for byte accounting. Exactness
// is not the goal; bounding growth is.
func (e *Entry) Size() int64 {
	size := int64(len(e.Body)) + 64
	for k, vs := range e.Header {
		size += int64(len(k)) + 32
		for _, v := range vs {
			size += int64(len(v))
		}
	}
	return size
}

// Clone returns a deep copy. Callers receive copies so a handler cannot mutate a
// shared cached entry and corrupt every subsequent response served from it.
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	c := *e
	c.Body = append([]byte(nil), e.Body...)
	if e.Header != nil {
		c.Header = e.Header.Clone()
	}
	return &c
}

// Cache is the storage contract. Implementations must be safe for concurrent use.
//
// Set must not return until the write is durable enough to be visible to a
// subsequent Get. The caching middleware relies on this: it calls Set before
// releasing waiting requests, so a Set that completed asynchronously would
// reintroduce the very race the middleware exists to close.
type Cache interface {
	Get(ctx context.Context, key string) (*Entry, bool, error)
	// Set stores entry under key. A ttl of 0 means the entry never expires.
	Set(ctx context.Context, key string, entry *Entry, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	// Ping reports backend reachability for readiness checks.
	Ping(ctx context.Context) error
	Stats() Stats
	Name() string
	Close() error
}

// Stats is a point-in-time snapshot used for logging and the readiness endpoint.
type Stats struct {
	Name            string `json:"name"`
	Hits            uint64 `json:"hits"`
	Misses          uint64 `json:"misses"`
	Sets            uint64 `json:"sets"`
	Evictions       uint64 `json:"evictions"`
	Expirations     uint64 `json:"expirations"`
	Rejected        uint64 `json:"rejected"`
	Errors          uint64 `json:"errors"`
	Entries         int    `json:"entries"`
	NegativeEntries int    `json:"negative_entries"`
	Bytes           int64  `json:"bytes"`
}

// HitRatio is the headline number: it is a direct proxy for money not spent.
func (s Stats) HitRatio() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}
