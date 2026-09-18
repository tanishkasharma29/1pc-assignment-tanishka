package cachemw

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/lock"
)

// instance models one replica: its own process-local cache and its own
// singleflight group, sharing a Redis with its peers.
func newInstance(t *testing.T, addr string, withLock bool) *Middleware {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close() })

	local := cache.NewMemory(cache.MemoryOptions{MaxEntries: 64, MaxNegativeEntries: 16})
	t.Cleanup(func() { _ = local.Close() })
	shared := cache.NewRedisWithClient(client, "typecode:")

	var locker lock.Locker = lock.NoOp{}
	if withLock {
		locker = lock.NewRedis(client, "typecode:", 10*time.Second, discardLogger())
	}

	return New(cache.NewTiered(local, shared, discardLogger()), locker, Options{
		Enabled:          true,
		MaxBodyBytes:     1 << 20,
		StoreHeaders:     []string{"Content-Type"},
		FetchTimeout:     5 * time.Second,
		LockWaitTimeout:  3 * time.Second,
		LockPollInterval: 5 * time.Millisecond,
	}, discardLogger())
}

// TestTwoInstancesMakeOneUpstreamCall is the reason Redis is in this design.
//
// singleflight stops at the process boundary, so two replicas with cold caches
// would each elect their own leader and each pay for the same data. A shared
// entry store plus a distributed lock collapses that back to one call for the
// fleet. Note that the dataset is ~51 KB and fits in either process by itself:
// Redis is here for coordination, never for capacity.
func TestTwoInstancesMakeOneUpstreamCall(t *testing.T) {
	server := miniredis.RunT(t)

	instanceA := newInstance(t, server.Addr(), true)
	instanceB := newInstance(t, server.Addr(), true)

	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	// One shared handler stands in for the single billable upstream.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})

	handlerA := instanceA.For(immutablePolicy())(handler)
	handlerB := instanceB.For(immutablePolicy())(handler)

	recA, recB := httptest.NewRecorder(), httptest.NewRecorder()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		handlerA.ServeHTTP(recA, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	}()

	<-entered // A holds the lock and is mid-fetch

	wg.Add(1)
	go func() {
		defer wg.Done()
		handlerB.ServeHTTP(recB, httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	}()

	time.Sleep(100 * time.Millisecond) // B discovers the lock is held and waits
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times across two instances; want 1", got)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{"A": recA, "B": recB} {
		if rec.Code != http.StatusOK {
			t.Errorf("instance %s: status = %d, want 200", name, rec.Code)
		}
		if body := rec.Body.String(); body != `{"id":1}` {
			t.Errorf("instance %s: body = %q", name, body)
		}
	}

	if stats := instanceB.Stats(); stats.PeerFills != 1 {
		t.Errorf("instance B peer_fills = %d, want 1: it should have been served by A's fetch", stats.PeerFills)
	}
}

// TestWithoutTheLockEachInstancePays is the contrast that justifies the lock.
//
// Same two instances, same shared Redis, lock disabled. Each elects its own
// leader and each pays. Sharing a cache is not sufficient on its own, because
// both instances miss before either has written.
func TestWithoutTheLockEachInstancePays(t *testing.T) {
	server := miniredis.RunT(t)

	instanceA := newInstance(t, server.Addr(), false)
	instanceB := newInstance(t, server.Addr(), false)

	var calls atomic.Int64
	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	})

	handlerA := instanceA.For(immutablePolicy())(handler)
	handlerB := instanceB.For(immutablePolicy())(handler)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		handlerA.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	}()
	go func() {
		defer wg.Done()
		handlerB.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/1", nil))
	}()

	<-entered
	<-entered // both instances reached the handler independently
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream called %d times; want 2, which is exactly the duplication the lock removes", got)
	}
}

// TestSecondInstanceReadsTheSharedEntry covers the sequential case: once one
// replica has fetched, another serves from the shared tier without its own call.
func TestSecondInstanceReadsTheSharedEntry(t *testing.T) {
	server := miniredis.RunT(t)

	instanceA := newInstance(t, server.Addr(), true)
	instanceB := newInstance(t, server.Addr(), true)

	var calls atomic.Int64
	handler := countingHandler(&calls, `{"id":1}`, http.StatusOK)

	handlerA := instanceA.For(immutablePolicy())(handler)
	handlerB := instanceB.For(immutablePolicy())(handler)

	handlerA.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts/1", nil))

	rec := httptest.NewRecorder()
	handlerB.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/posts/1", nil))

	if got := rec.Header().Get("X-Cache"); got != StatusHit {
		t.Errorf("instance B: X-Cache = %q, want HIT from the shared tier", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream called %d times; want 1", got)
	}
}
