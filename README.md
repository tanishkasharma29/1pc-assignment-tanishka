# Caching Proxy for JSONPlaceholder

A Go HTTP proxy in front of `jsonplaceholder.typicode.com` that makes the upstream
pay-per-call API cost as little as possible.

The routes are plumbing. The product is the caching middleware, and the property
it guarantees:

> For any given request, **at most one** upstream call is ever made. Every other
> request — an hour later or in the same microsecond — is served from the cache.

```bash
make run     # :8080, memory cache, no Docker and no Redis needed
make test    # 113 tests under -race, no network access required
make prove   # fire 200 concurrent cold requests, count the upstream calls
```

---

## Contents

- [The problem](#the-problem)
- [Quick start](#quick-start)
- [API](#api)
- [How the guarantee is achieved](#how-the-guarantee-is-achieved)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Running with Docker](#running-with-docker)
- [Testing](#testing)
- [Design decisions](#design-decisions)

---

## The problem

The upstream bills per invocation and returns immutable data per id. So the
objective is not "serve four routes" — it is **minimise upstream invocations, and
make that number provably bounded.**

Everything below is justified against that single metric.

---

## Quick start

Requires Go 1.26. Nothing else.

```bash
make run
```

```bash
curl -i localhost:8080/posts/1     # X-Cache: MISS   (one upstream call)
curl -i localhost:8080/posts/1     # X-Cache: HIT    (zero upstream calls)

curl -s localhost:8080/readyz | jq .cache
```

Watch the guarantee hold under concurrency:

```bash
make prove
```

```
firing 200 concurrent requests at http://localhost:8080/posts/14

requests sent          : 200
responses with 200     : 200
X-Cache: MISS          : 1   <- requests that performed the fetch
X-Cache: HIT           : 199
upstream calls made    : 1

PASS  200 concurrent cold requests were served by a single fetch
```

---

## API

| Method | Route | Upstream | Cached |
|---|---|---|---|
| `GET` | `/posts` | `/posts` | 10m |
| `GET` | `/posts/{id}` | `/posts/:id` | never expires |
| `GET` | `/todos` | `/todos` | 10m |
| `GET` | `/todos/{id}` | `/todos/:id` | never expires |
| `GET` | `/healthz` | — | liveness, touches no dependency |
| `GET` | `/readyz` | — | readiness plus cache and budget stats |

Items never expire because the problem statement guarantees they are immutable.
Collections get a finite TTL because a collection is *not* immutable in general —
adding a post would change `/posts`. Both are one line of config.

### Query parameters

| Route | Accepted |
|---|---|
| `/posts` | `userId` |
| `/todos` | `userId`, `completed` |

Three classes of parameter, handled differently, because the upstream itself
treats them differently:

| Class | Example | Behaviour |
|---|---|---|
| **Accepted** | `?userId=1` | validated, forwarded, part of the cache key |
| **Unrecognised** | `?foo=bar` | **silently stripped** |
| **Declined** | `?_limit=5` | **`400`** |

Unrecognised parameters are stripped rather than rejected because the upstream
ignores them and returns an identical response — so stripping is exact fidelity,
and it collapses `?x=1`, `?x=2`, `?x=3`… from unlimited billable cache keys into
a single cache hit.

`_limit`, `_start`, `_page`, `_sort` and `_order` are *honoured* by the upstream,
so stripping them would return 100 items to a client that asked for 5. A wrong
answer is worse than an honest error, so those are rejected.

### Response headers

| Header | Meaning |
|---|---|
| `X-Cache` | `HIT`, `MISS`, `HIT-COALESCED` or `BYPASS` |
| `X-Cache-Age` | seconds since the entry was stored |
| `X-Request-Id` | echoed if supplied, generated otherwise |
| `ETag` | supports `If-None-Match` → `304` |

### Errors

One envelope for every failure:

```json
{
  "error": {
    "code": "bad_request",
    "message": "invalid resource id",
    "fields": { "id": "must be an integer" }
  },
  "request_id": "9f2c1a7b4e0d8a3f5c6b2e10"
}
```

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | rejected at the edge — **costs nothing upstream** |
| `not_found` | 404 | upstream has no such resource (cached) |
| `rate_limited` | 429 | per-client limit exceeded |
| `upstream_error` | 502 | upstream failed |
| `upstream_timeout` | 504 | upstream too slow |
| `upstream_budget_exhausted` | 503 | we declined to spend |

---

## How the guarantee is achieved

### 1. Single-flight, not just caching

A plain read-through cache fails this requirement, and not as a corner case — as
the normal case under load:

```
t=0ms   100 requests for /posts/1 arrive
t=0ms   all 100 check the cache -> all 100 MISS (nobody has filled it yet)
t=0ms   all 100 call upstream          <-- 100 billable calls
t=40ms  all 100 write the same entry
```

Check-then-act is a race whenever the window between "check" and "fill" contains
another request. At any real concurrency it always does. This is a cache stampede.

`singleflight` admits one leader per key. Everyone else parks and is released
when the leader finishes:

```
t=0ms   100 requests for /posts/1 arrive
t=0ms   1 leader elected, 99 followers park on the key
t=0ms   leader calls upstream          <-- 1 billable call
t=40ms  leader fills cache, releases followers
t=40ms  99 followers read from cache   <-- 0 billable calls
```

Followers then **re-read the cache** rather than being handed the leader's
in-memory value, which is what makes "served from the cache" literally true for
every request after the first rather than merely true in effect.

### 2. The cache write completes before the response is flushed

**This ordering is the guarantee.** Reversed, single-flight only makes duplicate
calls unlikely rather than impossible:

```
leader: fetch -> write response to client -> cache.Set
                        |                         |
                        |    client receives, immediately requests again
                        |    request #2 arrives HERE -> MISS -> 2nd billable call
                        +-------------------------+
                          the race window
```

The window is small, but it is exactly where a fast client, a retry or a load
generator lands. The enforced order is:

```
fetch upstream
  -> buffer the complete response
  -> cache.Set(...) and AWAIT its acknowledgement
  -> return from singleflight.Do   (this releases the followers)
  -> only now write bytes to the leader's ResponseWriter
```

Two consequences, both deliberate:

- **The response is fully buffered, never streamed.** You cannot flush
  incrementally and still hold the invariant. This is why `recorder` buffers
  instead of writing through, and why it deliberately does **not** implement
  `http.Flusher` — advertising a capability it cannot honour would let a handler
  silently break the ordering.
- **`Set` is synchronous and acknowledged**, never fire-and-forget. For Redis
  that means awaiting the server's reply.

`Set` also happens *inside* the singleflight closure. After it, followers would
wake to an empty cache — the same race, moved.

`TestCacheIsPopulatedBeforeResponseIsFlushed` asserts this directly: it issues a
second request at the exact moment the first response begins being written, and
requires a `HIT`.

### 3. Redis is for coordination, not capacity

The entire dataset is **51,831 bytes** (100 posts + 200 todos, measured). One
process holds all of it with room to spare. Redis is not justified as a capacity
decision, and this design does not pretend otherwise.

Its one job: `singleflight` coordinates goroutines within a process and nothing
across processes, so N replicas with cold caches make **N** calls for the same
key. A distributed lock plus a shared entry store collapses that back to one.

- **L1 memory — always on.** The capacity layer. Sufficient alone.
- **L2 Redis — optional, off by default.** The coordination layer. One env var:
  `CACHE_BACKEND=redis`.

`TestTwoInstancesMakeOneUpstreamCall` proves it, and
`TestWithoutTheLockEachInstancePays` shows the duplication that appears without it.

### 4. Cost is bounded, not merely reduced

"Reduced" is what a cache gives you. **Bounded** requires closing every path by
which cost can be driven up:

| Vector | Defence | Residual cost |
|---|---|---|
| **Repetition** | cache | 1 call per key |
| **Concurrency** | single-flight + distributed lock | 1 call per key, fleet-wide |
| **Unknown ids** | edge validation + bounded negative cache | 0, or 1 per key |
| **Abuse** | param stripping + per-client limit + **global budget** | hard ceiling |

The last two are where naive implementations leak money.

**Unknown ids.** Probing confirmed the upstream answers `/posts/abc`, `/posts/0`,
`/posts/-1` and `/posts/99999` with a billable `404` — it does *not* reject
malformed input cheaply. So validation at the edge turns a paid round trip into a
local `400`, and 404s that are genuinely in range are **cached**, because
otherwise a trivial loop bills us forever.

But 404 keys are attacker-controlled and unbounded in cardinality, so they live in
a **separate hard-capped LRU partition**. Sharing one LRU would let 404 spam evict
the real dataset and turn the cache into a liability.
(`TestNegativePartitionDoesNotEvictPositiveEntries`.)

**Abuse.** A per-client token bucket limits one caller; it does not bound total
cost across many callers. The actual ceiling is a **global upstream budget**. It
is not a number invented at design time: the upstream publishes
`x-ratelimit-limit: 1000` and `x-ratelimit-remaining` on every response, so the
proxy reads its real allowance and refuses to spend the last of it.

---

## Architecture

```
cmd/server/                 wiring, signal handling, graceful shutdown, self-healthcheck
internal/
  config/                   defaults < YAML < env; per-route policy table
  apperror/                 typed errors -> one JSON envelope
  model/                    Post, Todo; validated before anything is cached
  upstream/                 typicode client: timeouts, bounded retry, size caps, budget
  service/                  maps upstream outcomes to application meaning
  cache/                    Cache interface + memory (L1) + redis (L2) + tiered
  lock/                     Locker interface + redis SET NX; no-op by default
  cachemw/       <-- the caching middleware
  httpmw/                   request id, logging, recovery, timeout, rate limit, query sanitiser
  transport/httpapi/        router, handlers, validation, response rendering
deploy/                     config.yaml, nginx.conf
scripts/prove.sh            the concurrency demonstration
```

**The middleware knows nothing about posts or todos.** It operates only on
`http.Request` and `http.ResponseWriter`, and its own tests drive it through a
stub handler. Adding `GET /comments/{id}` is one route registration and one policy
entry, with no change to `cachemw` at all.

### Middleware order

Order is a correctness concern here, not a style choice:

```
RequestID -> Logger -> Recoverer -> Timeout -> RateLimit
          -> SanitizeQuery -> ValidateID -> Cache -> handler
```

- `Logger` sits outside `Recoverer` so a recovered panic still produces a log line.
- `Recoverer` sits **outside the cache middleware** because `singleflight`
  propagates a leader's panic to every request waiting on the same key. Without
  an outer recovery, one bad response takes down every concurrent caller.
- `SanitizeQuery` and `ValidateID` run **before** the cache so invalid requests
  never build a key, and the query reaching the cache is already canonical.

### Dependencies

`chi` (router), `x/sync` (singleflight), `x/time` (rate limiter), `go-redis`,
`yaml.v3`, `miniredis` (tests). Everything else is standard library — `log/slog`
for structured logging, `net/http` for the server.

---

## Configuration

Precedence: **defaults < `deploy/config.yaml` < environment**. The service runs
correctly with no file and no environment at all.

The YAML holds the per-route policy table, which is hierarchical and reads badly
as flat env vars. Unknown fields are **rejected** rather than ignored, because a
silently dropped typo is how misconfiguration hides.

### Per-route cache policy

```yaml
cache:
  routes:
    posts_item:
      enabled: true
      ttl: "0s"            # never expires: immutable
      negative_ttl: "5m"
    posts_list:
      enabled: true
      ttl: "10m"
      allow_query: ["userId"]
      deny_query: ["_limit", "_start", "_page", "_sort", "_order"]
```

Note the deliberate asymmetry: `ttl: 0` means *never expires*, but
`negative_ttl: 0` means *do not cache negatives at all*. Positive keys come from a
small set we validate; negative keys are chosen by the caller and unbounded, so
making them immortal would hand out unlimited memory to anyone who asked.

### Environment variables

| Variable | Default | Notes |
|---|---|---|
| `SERVER_ADDR` | `:8080` | |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | |
| `UPSTREAM_BASE_URL` | jsonplaceholder | |
| `UPSTREAM_TIMEOUT` | `3s` | per attempt |
| `UPSTREAM_MAX_ATTEMPTS` | `3` | |
| `UPSTREAM_BUDGET_ENABLED` | `true` | |
| `UPSTREAM_BUDGET_MAX_CALLS` | `800` | per window |
| `CACHE_ENABLED` | `true` | global kill switch |
| `CACHE_BACKEND` | `memory` | `memory` \| `redis` |
| `CACHE_DEBUG_HEADERS` | `false` | adds `X-Cache-Key`; never in production |
| `REDIS_ADDR` | `localhost:6379` | |
| `REDIS_LOCK_ENABLED` | `true` | cross-instance coordination |
| `VALIDATION_MAX_RESOURCE_ID` | `1000` | bounds the keyspace |
| `VALIDATION_MAX_USER_ID` | `100` | |
| `RATE_LIMIT_ENABLED` | `true` | |
| `RATE_LIMIT_TRUST_PROXY` | `false` | only enable behind a proxy you control |

**Per-route overrides** — disable caching for one route without a redeploy:

```bash
CACHE_ROUTE_TODOS_LIST_ENABLED=false
CACHE_ROUTE_POSTS_ITEM_TTL=30m
CACHE_ROUTE_POSTS_ITEM_NEGATIVE_TTL=1m
```

---

## Running with Docker

Requires Docker with Compose v2. No local Go toolchain needed.

```bash
make docker-up      # 2 containers: app + Redis
make docker-prove   # 200 concurrent cold requests, count upstream calls
make docker-logs
make docker-down
```

Two containers:

| Container | Image | Port | Role |
|---|---|---|---|
| `app` | built here | `8080:8080` | the proxy |
| `redis` | `redis:7-alpine` | internal only | shared entry store + distributed lock |

The first `docker-up` takes a few minutes: the image build runs the full test
suite under `-race`, so a broken commit cannot produce a runnable image. Skip it
while iterating with `docker compose build --build-arg RUN_TESTS=0`.

### What Redis is and is not doing here

With a single replica, `singleflight` already guarantees one upstream call per
key — Redis is not what makes that true, and the proof below would pass without
it. Redis is wired in as the shared entry store and lock that a *second* replica
would need, so scaling out is a compose change rather than a code change.

It is worth being plain about this: the entire dataset is ~51 KB and fits in the
process. Redis is never a capacity decision in this design. It earns its place
only when more than one instance is running, which is what the optional cluster
setup below demonstrates.

```bash
make docker-prove
```

```
firing 200 concurrent requests at http://localhost:8080/posts/57

requests sent          : 200
responses with 200     : 200
X-Cache: MISS          : 1
X-Cache: HIT           : 199
upstream calls (fleet) : 1

PASS  200 concurrent cold requests were served by a single fetch
```

Inspect what landed in Redis:

```bash
make docker-redis
> KEYS typecode:*
> DBSIZE
> TTL typecode:entry:<hash>     # -1 means never expires, correct for immutable items
```

### Optional: the multi-instance proof

The single-replica setup cannot demonstrate the one thing Redis is actually for.
This overlay adds a second replica and an nginx balancer:

```bash
make docker-up-cluster   # 4 containers; balancer on :8000, replicas on :8080 and :8082
make prove-cluster
make docker-down-cluster
```

```
firing 200 concurrent requests at http://localhost:8000/posts/57
counting upstream calls across 2 replicas

X-Cache: MISS          : 1
upstream calls (fleet) : 1
served by              : 101 172.19.0.3:8080   99 172.19.0.4:8080

PASS
```

The `served by` line is the point: traffic genuinely reached both replicas, so the
single call was a fleet-wide result rather than an artefact of everything landing
on one instance.

To see the contrast, disable the lock and repeat — each replica then pays for its
own first call, because both miss before either has written:

```bash
docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  stop app app-2
REDIS_LOCK_ENABLED=false docker compose -f docker-compose.yml -f docker-compose.cluster.yml \
  up -d app app-2
make prove-cluster        # upstream calls (fleet) : 2
```

A shared cache alone is not sufficient. That is the argument for the lock.

### Image details

Multi-stage. The runtime layer is `distroless/static` — no shell, no package
manager, non-root (uid 65532), static binary. Since the image has no `curl`, the
server ships its own probe (`server -healthcheck`) rather than the image carrying
tools it would otherwise never need; Compose gates startup on that probe.

The build stage installs `gcc` and `musl-dev`, which the race detector needs —
the Alpine Go image carries no C toolchain, and `go test -race` fails without them.

### Configuration in containers

Compose sets these; everything else comes from the baked-in `config.yaml`:

| Variable | Value | Why |
|---|---|---|
| `CACHE_BACKEND` | `redis` | selects the tiered memory + Redis cache |
| `REDIS_ADDR` | `redis:6379` | |
| `REDIS_LOCK_ENABLED` | `true` | inert with one replica; ready for two |
| `RATE_LIMIT_ENABLED` | `false` | the proof fires 200 requests from one address; the limiter would otherwise shape the burst being measured |
| `RATE_LIMIT_TRUST_PROXY` | `true` *(cluster only)* | safe only because nginx sits in front |

## Testing

```bash
make test        # everything, with -race
make bench-proof # only the tests that prove the single-call guarantee
make cover       # coverage report -> coverage.html
```

113 tests, **74.4%** statement coverage. No test touches the real upstream; Redis
paths use `miniredis`, so the suite is hermetic and needs no network.

| Test | What it proves |
|---|---|
| `TestSingleFlightConcurrentColdKey` | 200 concurrent cold requests → **exactly 1** upstream call |
| `TestCacheIsPopulatedBeforeResponseIsFlushed` | the write-before-flush ordering, made deterministic |
| `TestLeaderCancellationDoesNotFailFollowers` | a disconnecting leader does not fail 99 waiters |
| `TestTwoInstancesMakeOneUpstreamCall` | cross-instance coordination via Redis |
| `TestWithoutTheLockEachInstancePays` | the duplication the lock removes |
| `TestRejectedRequestsCostNothing` | 15 invalid requests → **0** upstream calls |
| `TestUnknownParametersShareTheBareRouteEntry` | cache-busting params become hits |
| `TestNegativePartitionDoesNotEvictPositiveEntries` | 404 spam cannot flush the real dataset |
| `TestReleaseOnlyDeletesOwnLock` | an overrun leader cannot free a peer's lock |
| `TestServerErrorsAreNotCached` | a 5xx is never stored and replayed |

Coverage by package:

| Package | Coverage |
|---|---|
| `apperror`, `model` | 100.0% |
| `upstream` | 89.0% |
| `httpmw`, `service` | 86.0% |
| `transport/httpapi` | 85.5% |
| `cachemw` | 79.8% |
| `config` | 79.0% |
| `lock` | 76.2% |
| `cache` | 68.5% |

---

## Design decisions

Things that could reasonably have gone another way, and why they did not.

**Buffering the whole response instead of streaming.** Required by the ordering
guarantee — you cannot flush incrementally and still store before the first byte
leaves. Bounded by `max_body_bytes` plus the upstream client's own size cap.
Oversized responses are served but not cached.

**The leader's fetch runs on a detached context.** If the client that happened to
be elected leader disconnects, inheriting its cancellation would abort the fetch,
fail every waiter, and discard a call that may already have been billed.
`context.WithoutCancel` keeps the request-scoped values while dropping a
cancellation that does not belong to the followers.

**404s are cached; 5xx are not.** For immutable data a 404 is a permanent, correct
answer. A 5xx is transient, and storing it would serve a failure long after the
cause was fixed.

**Empty results are successes.** The upstream returns `200 []` for an unmatched
filter. Cacheability is decided on status code alone, never on body emptiness.

**Typed models rather than opaque byte proxying.** Decoding costs something, but
only on a miss — which the whole system exists to make rare — and it means a
degraded upstream response is caught before it can be cached and served for the
lifetime of an immutable entry.

**A stale lock is released by compare-and-swap, not `DEL`.** A leader that
overran its TTL must not delete a lock another instance has since taken, or two
instances would fetch at once — reintroducing exactly what the lock prevents.

**Follower waiting fails open.** If a peer holding the lock dies, waiters give up
after a bounded interval and fetch themselves. Availability is chosen over the
strict cost bound deliberately; the budget remains the real ceiling.

**Pagination parameters are declined, not supported.** Supporting them makes the
keyspace combinatorial and the bound degrades to "whatever the budget allows"
instead of a provable number. The mechanism supports adding them with validated
ranges if needed — this is a chosen default, not a limitation.
