package cache

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Memory is an in-process LRU cache with TTL support.
//
// It holds two independent LRU partitions. This is not an optimisation, it is a
// containment boundary: keys that produce 2xx responses are drawn from a small
// finite set we control, whereas keys that produce 404s are chosen by the
// caller and unbounded in cardinality. Sharing one LRU would let a trivial loop
// over nonexistent IDs evict the entire real dataset and turn the cache from an
// asset into a liability.
type Memory struct {
	mu       sync.Mutex
	positive *lruList
	negative *lruList
	maxBytes int64
	bytes    int64

	hits        atomic.Uint64
	misses      atomic.Uint64
	sets        atomic.Uint64
	evictions   atomic.Uint64
	expirations atomic.Uint64
	rejected    atomic.Uint64

	now       func() time.Time
	stop      chan struct{}
	closeOnce sync.Once
}

// MemoryOptions configures a Memory cache. Zero values are replaced by sane
// defaults so a partially specified struct cannot produce an unbounded cache.
type MemoryOptions struct {
	MaxEntries         int
	MaxNegativeEntries int
	MaxBytes           int64
	JanitorInterval    time.Duration
	// Now is injectable so TTL behaviour can be tested deterministically rather
	// than by sleeping.
	Now func() time.Time
}

func NewMemory(opts MemoryOptions) *Memory {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 4096
	}
	if opts.MaxNegativeEntries <= 0 {
		opts.MaxNegativeEntries = 1024
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 64 << 20
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	m := &Memory{
		positive: newLRUList(opts.MaxEntries),
		negative: newLRUList(opts.MaxNegativeEntries),
		maxBytes: opts.MaxBytes,
		now:      opts.Now,
		stop:     make(chan struct{}),
	}
	if opts.JanitorInterval > 0 {
		go m.janitor(opts.JanitorInterval)
	}
	return m
}

func (m *Memory) Name() string { return "memory" }

func (m *Memory) Get(_ context.Context, key string) (*Entry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	part, node := m.locate(key)
	if node == nil {
		m.misses.Add(1)
		return nil, false, nil
	}
	if node.entry.Expired(m.now()) {
		m.removeLocked(part, node)
		m.expirations.Add(1)
		m.misses.Add(1)
		return nil, false, nil
	}

	part.touch(node)
	m.hits.Add(1)
	// Cloned so callers cannot mutate shared state.
	return node.entry.Clone(), true, nil
}

func (m *Memory) Set(_ context.Context, key string, entry *Entry, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	stored := entry.Clone()
	now := m.now()
	stored.StoredAt = now
	if ttl > 0 {
		stored.ExpiresAt = now.Add(ttl)
	} else {
		stored.ExpiresAt = time.Time{} // never expires
	}

	size := stored.Size()

	m.mu.Lock()
	defer m.mu.Unlock()

	// An entry that cannot fit even in an empty cache is refused outright.
	// Admitting it would evict everything else and still not be retainable.
	if size > m.maxBytes {
		m.rejected.Add(1)
		return nil
	}

	// Replace any existing copy, including one in the other partition: a status
	// change would otherwise leave a stale duplicate behind.
	if part, node := m.locate(key); node != nil {
		m.removeLocked(part, node)
	}

	part := m.partitionFor(stored)
	part.pushFront(&lruNode{key: key, entry: stored, size: size})
	m.bytes += size
	m.sets.Add(1)

	m.evictLocked(part)
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if part, node := m.locate(key); node != nil {
		m.removeLocked(part, node)
	}
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Stats() Stats {
	m.mu.Lock()
	entries, negative, bytes := m.positive.len(), m.negative.len(), m.bytes
	m.mu.Unlock()

	return Stats{
		Name:            m.Name(),
		Hits:            m.hits.Load(),
		Misses:          m.misses.Load(),
		Sets:            m.sets.Load(),
		Evictions:       m.evictions.Load(),
		Expirations:     m.expirations.Load(),
		Rejected:        m.rejected.Load(),
		Entries:         entries,
		NegativeEntries: negative,
		Bytes:           bytes,
	}
}

func (m *Memory) Close() error {
	m.closeOnce.Do(func() { close(m.stop) })
	return nil
}

// locate finds a key in either partition. Callers must hold m.mu.
func (m *Memory) locate(key string) (*lruList, *lruNode) {
	if node := m.positive.get(key); node != nil {
		return m.positive, node
	}
	if node := m.negative.get(key); node != nil {
		return m.negative, node
	}
	return nil, nil
}

func (m *Memory) partitionFor(e *Entry) *lruList {
	if e.Positive() {
		return m.positive
	}
	return m.negative
}

// evictLocked enforces both bounds: each partition's entry count, then the
// shared byte ceiling. Callers must hold m.mu.
func (m *Memory) evictLocked(inserted *lruList) {
	for inserted.len() > inserted.max {
		if node := inserted.back(); node != nil {
			m.removeLocked(inserted, node)
			m.evictions.Add(1)
		}
	}
	// Under byte pressure the negative partition is drained first: its contents
	// are cheap to re-derive and were requested by nobody we owe latency to.
	for m.bytes > m.maxBytes {
		part := m.negative
		if part.len() == 0 {
			part = m.positive
		}
		node := part.back()
		if node == nil {
			break
		}
		m.removeLocked(part, node)
		m.evictions.Add(1)
	}
}

func (m *Memory) removeLocked(part *lruList, node *lruNode) {
	part.remove(node)
	m.bytes -= node.size
	if m.bytes < 0 {
		m.bytes = 0
	}
}

// janitor reclaims entries that expired but were never read again. Expiry is
// also enforced on read, so this only bounds memory held by cold keys.
func (m *Memory) janitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

func (m *Memory) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, part := range []*lruList{m.positive, m.negative} {
		for _, node := range part.snapshot() {
			if node.entry.Expired(now) {
				m.removeLocked(part, node)
				m.expirations.Add(1)
			}
		}
	}
}

// lruNode is one cache entry plus its bookkeeping.
type lruNode struct {
	key   string
	entry *Entry
	size  int64
	elem  *list.Element
}

// lruList is a plain LRU: a doubly linked list ordered by recency plus a map for
// O(1) lookup. Not independently locked — Memory owns the single mutex, which
// keeps the two-partition invariants (shared byte total) trivially consistent.
type lruList struct {
	max   int
	ll    *list.List
	items map[string]*lruNode
}

func newLRUList(max int) *lruList {
	return &lruList{max: max, ll: list.New(), items: make(map[string]*lruNode, max)}
}

func (l *lruList) len() int { return l.ll.Len() }

func (l *lruList) get(key string) *lruNode { return l.items[key] }

func (l *lruList) touch(node *lruNode) { l.ll.MoveToFront(node.elem) }

func (l *lruList) pushFront(node *lruNode) {
	node.elem = l.ll.PushFront(node)
	l.items[node.key] = node
}

func (l *lruList) remove(node *lruNode) {
	l.ll.Remove(node.elem)
	delete(l.items, node.key)
}

func (l *lruList) back() *lruNode {
	elem := l.ll.Back()
	if elem == nil {
		return nil
	}
	return elem.Value.(*lruNode)
}

// snapshot copies the nodes so callers can remove during iteration.
func (l *lruList) snapshot() []*lruNode {
	out := make([]*lruNode, 0, len(l.items))
	for _, node := range l.items {
		out = append(out, node)
	}
	return out
}
