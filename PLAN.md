# Caching Proxy for JSONPlaceholder — Design Plan

**Status:** proposed, not yet implemented
**Language:** Go 1.24 · **Runtime:** local-first (`make run`, no infra required)

---

## 1. Problem

Proxy four upstream endpoints from `jsonplaceholder.typicode.com`:

| Our route | Upstream |
|---|---|
| `GET /posts` | `/posts` |
| `GET /posts/{id}` | `/posts/:id` |
| `GET /todos` | `/todos` |
| `GET /todos/{id}` | `/todos/:id` |

The upstream bills **per invocation** and returns **immutable data per ID**.

So the objective function is not "serve these routes." It is:

> **Minimise the number of upstream invocations, and make that number provably bounded.**

Every decision below is justified against that single metric. The proxy itself is
plumbing; the cache layer is the product.

---

## 1a. Verified upstream contract

Probed live on 2026-09-18; the design below rests on these facts, not assumptions.

**Payload shapes**

```jsonc
// GET /posts/1                      // GET /todos/1
{ "userId": 1, "id": 1,              { "userId": 1, "id": 1,
  "title": "sunt aut facere...",       "title": "delectus aut autem",
  "body":  "quia et suscipit..." }     "completed": false }
```

**Size and cardinality** — confirms G3:

| | items | bytes |
|---|---|---|
| `/posts` | 100 | 27,520 |
| `/todos` | 200 | 24,311 |
| **total** | **300** | **51,831 (~51 KB)** |

**Observed behaviour**

| Probe | Result | Consequence for the design |
|---|---|---|
| `/posts/100`, `/todos/200` | `200` | ID ranges confirmed: posts 1–100, todos 1–200 |
| `/posts/101`, `/todos/201` | `404 {}` | Out-of-range is billable; negative caching required |
| `/posts/abc`, `/posts/0`, `/posts/-1` | `404 {}` | **Upstream bills for garbage input too** — it does not reject cheaply. Edge validation saves real calls, not hypothetical ones. |
| `/posts?userId=99` | `200 []` | Empty result is a **success**, not a 404. Must not be negative-cached. |
| `/posts?foo=bar` | `200`, full 27,520-byte collection | **Unknown params are silently ignored.** See §6.9 — this is the cache-busting vector, made concrete. |
| `/posts?_limit=5` | `200`, 5 items | `_limit`/`_start`/`_page` **are** honoured. Stripping them would return a wrong answer. |
| `x-ratelimit-limit: 1000` | header on every response | Upstream publishes a real budget. G4's global ceiling is reconciled with this rather than invented. |
| `Cache-Control: max-age=43200`, `etag`, `Age: 429` | present | Upstream has its own CDN and caching hints — but also emits `pragma: no-cache` and `expires: -1`, contradicting its own `max-age`. We ignore its hints and use our policy. |
| TTFB | 73–250 ms | The cache saves ~100 ms/request as well as money. Informs a 3 s upstream timeout. |

---

## 2. The four guarantees

These are the load-bearing claims. Everything in §3–§6 exists to support them.

### G1 — Single-flight, not just caching

A plain read-through cache **fails the bonus requirement.** The failure is not a
corner case, it is the normal case under load:

```
t=0ms   100 requests for /posts/1 arrive
t=0ms   all 100 check the cache -> all 100 MISS (nobody has filled it yet)
t=0ms   all 100 call upstream          <-- 100 billable calls
t=40ms  all 100 write the same entry
```

Check-then-act is a race whenever the window between "check" and "fill" contains
another request. At any real concurrency it always does. This is a cache stampede.

**Fix:** `golang.org/x/sync/singleflight`, keyed by the cache key. Exactly one
goroutine (the *leader*) executes the fetch per key; every other caller (a
*follower*) blocks on the same key and is released when the leader finishes.

```
t=0ms   100 requests for /posts/1 arrive
t=0ms   1 leader elected, 99 followers park on the key
t=0ms   leader calls upstream          <-- 1 billable call
t=40ms  leader fills cache, releases followers
t=40ms  99 followers read from cache   <-- 0 billable calls
```

Followers are released and then **re-read the cache** rather than being handed the
leader's in-memory value. That is what makes the requirement literally true: every
invocation except the first is served *from the cache*, not from a shared variable.
(If the cache read somehow misses — a failed `Set`, see §6 — they fall back to the
leader's value, so availability never depends on the cache. Only the cost guarantee does.)

### G2 — The cache write completes before the response is flushed

**This ordering is the guarantee.** Reverse it and G1 is only probabilistic.

The broken order:

```
leader: fetch -> write response to client -> cache.Set
                        |                         |
                        |    client receives, immediately requests again
                        |    request #2 arrives HERE -> MISS -> 2nd billable call
                        +-------------------------+
                          the race window
```

The window is small but it is exactly where a fast client, a retry, or a load
generator lands. "Usually one call" is not the requirement.

The correct order, which the middleware enforces:

```
fetch upstream
  -> buffer the complete response (status + headers + body)
  -> cache.Set(...) and AWAIT its acknowledgement
  -> return from singleflight.Do  (this releases the followers)
  -> only now write the buffered bytes to the leader's ResponseWriter
```

Two consequences fall out of this and both are deliberate:

- **The response must be fully buffered, not streamed.** You cannot flush
  incrementally and still hold the invariant. This is why the recorder captures
  into a buffer instead of writing through to the real `ResponseWriter`. Bounded
  by `cache.max_body_bytes` plus a hard `io.LimitReader` cap on the upstream read,
  so buffering cannot become a memory vector.
- **`cache.Set` must be synchronous and acknowledged**, never fire-and-forget. For
  Redis that means awaiting the server's reply, not pipelining it away.

The `Set` also happens **inside** the singleflight closure, before `Do` returns. If
it happened after, followers would wake to an empty cache — the same race, moved.

### G3 — Redis is for coordination, not capacity

The entire dataset is 100 posts + 200 todos = **51,831 bytes (~51 KB)**, measured. A single process holds all
of it in a map with room to spare. **Redis is not needed for capacity and is not
justified as a capacity decision.**

Its one job: when you run N replicas behind a load balancer, `singleflight` is
per-process, so N cold replicas make **N** upstream calls for the same key. Redis
collapses that back to 1, via a distributed lock plus a shared entry store that lets
the other N−1 instances fill their local cache from Redis instead of from upstream.

Therefore:

- **L1 (in-memory, always on)** is the capacity layer. It serves every hit. It is
  sufficient on its own for a single instance.
- **L2 (Redis, optional)** is the coordination layer. `CACHE_BACKEND=redis`,
  **off by default.** `make run` needs no Docker, no Redis, no network but the upstream.

Single-instance is the default path and is fully correct without Redis. Redis is
opt-in for the multi-replica case only, and `docker-compose` exists to demonstrate
exactly that.

### G4 — Cost is bounded, not merely reduced

"Reduced" is what a cache gives you. **Bounded** requires closing every path by
which an attacker or a bug can drive upstream calls. There are four, and each needs
its own defence:

| Vector | Defence | Residual cost |
|---|---|---|
| **Repetition** — same key requested many times | L1/L2 cache | 1 call per key |
| **Concurrency** — many simultaneous first-requests | single-flight (G1) + Redis lock (G3) | 1 call per key, fleet-wide |
| **Unknown IDs** — `/posts/99999`, `/posts/abc` | edge validation + bounded negative cache | 0, or 1 per key |
| **Abuse** — deliberate cache-busting | param allowlist + per-client rate limit + **global upstream budget** | hard ceiling |

The third and fourth deserve detail, because they are where naive implementations
leak money:

**Unknown IDs.** Upstream returns `404` for `/posts/99999` — and, confirmed by probe,
for `/posts/abc`, `/posts/0` and `/posts/-1` as well. It does **not** reject malformed
input cheaply; every one of those is a billable round trip. If only `2xx` is cached,
every repeat of that request is billable — unbounded cost from a single trivial loop.
So: **404 responses are cached** with their own shorter TTL. But 404 keys are
attacker-controlled and unbounded in cardinality, so they get a **separate,
hard-capped LRU partition** — otherwise 404 spam evicts the 300 real entries and
turns the cache into a liability. Before any of that, `/posts/abc` and `/posts/-1`
are rejected at the edge with `400` and **never reach the upstream at all**: the
cheapest call is the one you don't make.

**Abuse.** A per-client token bucket limits one bad actor; it does not bound total
cost across many clients. The actual ceiling is a **global upstream call budget** —
a counter of upstream invocations per window. When exhausted, the proxy serves
whatever it has and returns `503` for genuine misses rather than spending money.
This is the last line of defence and the only one that is a true bound. The budget is
not invented: upstream publishes `x-ratelimit-limit: 1000` with a `x-ratelimit-reset`
epoch on every response, so the proxy reads `x-ratelimit-remaining` from each fetch,
tracks it, and refuses to spend the last of it on speculative traffic.

**The resulting cost model** (see §5 for the derivation): the keyspace is *finite by
construction*, at **344 distinct keys**. Steady-state upstream calls: **300 for items,
ever.**

---

## 3. Architecture

```
cmd/server/                 wiring, signal handling, graceful shutdown
internal/
  config/                   env + YAML config, per-route cache policies
  apperror/                 typed errors -> consistent JSON envelope
  model/                    Post, Todo (typed; validates the upstream contract)
  upstream/                 typicode client: timeouts, bounded retry, size limits
  service/                  business logic; maps upstream status -> app errors
  cache/                    Cache interface + memory (L1) + redis (L2) + tiered
  lock/                     Locker interface + redis SET NX lock (no-op by default)
  cachemw/                  >>> the caching middleware (the core) <<<
  httpmw/                   request ID, structured logging, recover, timeout, rate limit
  transport/httpapi/        router, handlers, param validation, response writing
deploy/                     Dockerfile, docker-compose.yml, config.yaml
```

Dependencies, kept deliberately small: `chi` (router), `x/sync` (singleflight),
`go-redis/v9` (optional backend), `yaml.v3` (config). Everything else is stdlib —
`log/slog` for structured logging, `net/http` for the server.

**Layering rule:** the caching middleware knows nothing about posts or todos. It
operates on `http.Request` / `http.ResponseWriter` only. Adding `GET /comments/{id}`
tomorrow is one route registration and one policy entry — zero middleware changes.
That is the "generic" requirement, and it is testable: the middleware's own tests
use a fake handler, never the real service.

---

## 4. The caching middleware

### Lifecycle

```
GET /posts/1
  |
  ├─ policy lookup (per-route) -- disabled? -> pass straight through
  ├─ method != GET?            -> pass straight through
  |
  ├─ build key: method + path + canonical(sorted, allowlisted query)
  |
  ├─ L1 lookup ──────── HIT ──> write stored status/headers/body, X-Cache: HIT
  |                             (no upstream call)
  └─ MISS
       |
       └─ singleflight.Do(key):
            │  [LEADER — exactly one goroutine per key]
            │
            ├─ re-check L1        (another leader may have just filled it)
            ├─ check L2/Redis     (another *instance* may have filled it)
            ├─ acquire Redis lock (if enabled; else no-op)
            │    └─ not acquired -> poll L2 with backoff for the other
            │                       instance's result; on timeout, fail open
            │                       and fetch (availability > strict cost)
            │
            ├─ run next handler with a DETACHED context (§6) -> upstream call
            ├─ buffer the complete response
            ├─ cache.Set, AWAIT ack                          <-- G2 ordering
            └─ release lock, return  ────> releases followers
                                            |
       ┌────────────────────────────────────┘
       |
       ├─ LEADER    -> write buffered response, X-Cache: MISS
       └─ FOLLOWERS -> re-read cache -> write it, X-Cache: HIT
                       (fallback to leader's value only if the cache read fails)
```

Leader vs. follower is distinguished by a `bool` captured per call — `singleflight`
only invokes the closure in the leader's goroutine, so the follower's copy stays
`false`. (`Do`'s `shared` return value is *not* usable for this: it is `true` for the
leader too whenever anyone waited.)

### Cache key

`GET|/posts|userId=1` — method, path, then the query string **sorted** and
**filtered to the route's allowlist**, so `?a=1&b=2` and `?b=2&a=1` are one entry.
Optionally salted with configured `Vary` headers. Hashed (SHA-256, truncated) for
the Redis key; the readable form is kept for logs and for `X-Cache-Key` in debug mode.

### Per-route policy — the "configurable" requirement

Configurable at two levels, because "enable/disable for a particular route" can
reasonably mean either:

1. **Route level** — the middleware is only mounted where it is wanted:
   `r.With(cache.For(policy.PostsItem)).Get("/posts/{id}", h.GetPost)`
2. **Runtime level** — a policy table in `config.yaml`, overridable per route by
   env var (`CACHE_ROUTE_TODOS_LIST_ENABLED=false`), so caching can be turned off
   for one route in production **without a redeploy**.

```yaml
cache:
  enabled: true
  backend: memory          # memory | redis
  routes:
    posts_item:  { enabled: true, ttl: 0s,    negative_ttl: 5m }   # 0 = immutable
    posts_list:  { enabled: true, ttl: 10m,   negative_ttl: 1m, allow_query: [userId] }
    todos_item:  { enabled: true, ttl: 0s,    negative_ttl: 5m }
    todos_list:  { enabled: true, ttl: 10m,   negative_ttl: 1m, allow_query: [userId, completed] }
```

Items get `ttl: 0` (never expire) because the problem statement guarantees
immutability. Collections get a finite TTL because a collection is *not* immutable in
general — a new post would change `/posts`. That is a one-line change if the
assumption differs, and it is exactly the kind of thing per-route policy exists for.

---

## 5. Cost model

The keyspace is finite **by construction**, not by hope:

| Route | Validation at the edge | Distinct keys |
|---|---|---|
| `/posts/{id}` | `id` integer, `1 ≤ id ≤ 100` (probe-confirmed range) | 100 |
| `/todos/{id}` | `id` integer, `1 ≤ id ≤ 200` (probe-confirmed range) | 200 |
| `/posts` | allowlist `{userId}`, `1 ≤ userId ≤ 10` | 1 + 10 = 11 |
| `/todos` | allowlist `{userId, completed}` | 1 + 10 + 2 + 20 = 33 |
| | **Total** | **344** |

### Query params: a three-way rule, not an allowlist

The probe results force a more careful rule than "allowlist, reject the rest," because
upstream treats the two kinds of unsupported param *differently*:

| Class | Example | Handling | Why |
|---|---|---|---|
| **Allowed** | `userId`, `completed` | validated, in the cache key, forwarded | supported feature |
| **Unrecognised** | `foo=bar` | **stripped** from key and upstream request | upstream *ignores* these and returns the identical full collection. Stripping is exact fidelity **and** collapses the cache-busting keyspace to zero. Rejecting would be less faithful than upstream itself. |
| **Known but declined** | `_limit`, `_start`, `_page` | **`400`** | upstream *honours* these. Silently stripping them would return 100 items to a client that asked for 5 — a wrong answer, which is worse than an honest error. |

That middle row is the correction the probe bought us: my first instinct was to `400`
on unknown params, but since upstream ignores them, stripping is strictly better — it
keeps proxy fidelity *and* turns `?foo=1..N` from N billable calls into N cache hits.

Declining `_limit`/`_start`/`_page` is a deliberate tradeoff: supporting them makes the
keyspace combinatorial and the bound degrades to "whatever the budget allows" instead
of a provable number. If pagination is needed it moves to the allowlist with validated
ranges and the bound is recomputed — the mechanism already supports it.

**Steady state:**
- Items: `ttl: 0` → **300 upstream calls, ever** (per cache lifetime).
- Collections: 44 keys × finite TTL → 44 calls per TTL window; **344 ever** if
  collections are also declared immutable (one config line).

---

## 6. Failure modes and the decisions taken

These are the places this design is easy to get wrong. Listing them because the
reasoning matters more than the code.

| # | Failure mode | Decision |
|---|---|---|
| 1 | **Leader's client disconnects mid-fetch.** `r.Context()` is cancelled, the upstream call aborts, and *every follower* fails — plus the call is billed for nothing. | Leader fetches with `context.WithoutCancel(r.Context())` + its own timeout. One client hanging up cannot fail the other 99 or waste the invocation. |
| 2 | **`cache.Set` fails** (Redis down, entry too large). | Serve the response anyway. Log + count it. Availability never depends on the cache; only the cost guarantee does, and it degrades rather than breaking. |
| 3 | **Leader's fetch errors.** `singleflight` would let the next request join a doomed in-flight call. | `Forget(key)` on error, so the retry is a fresh attempt. Errors are never cached (except `404`, which is a valid immutable answer). |
| 4 | **Leader panics.** `singleflight` re-panics in every waiter. | `Recoverer` sits *outside* the cache middleware, so each waiter's request becomes a clean `500` rather than killing the process. |
| 5 | **Redis lock holder dies** holding the lock. | Lock has a TTL; followers poll with bounded backoff and then **fail open** and fetch themselves. Chooses availability over the strict cost bound, deliberately — the budget (G4) is the real ceiling. |
| 6 | **Negative-cache pollution** — 404 spam evicting the 300 real entries. | Separate hard-capped LRU partition for non-2xx entries. |
| 7 | **Caching the wrong headers** — `Date`, `Set-Cookie`, hop-by-hop. | Allowlist: `Content-Type`, `Content-Language`, `ETag`, `Last-Modified`, `Vary`. Nothing else is stored or replayed. |
| 9 | **Cache-busting via ignored params** — `?foo=1..N` yields N distinct keys for byte-identical 27 KB responses. | Unrecognised params stripped from key *and* upstream request (§5). `?foo=bar` becomes a HIT on the `/posts` entry. |
| 10 | **Empty result mistaken for an error** — `?userId=99` returns `200 []`. | Cacheability is decided on status code, never on body emptiness. `200 []` is a normal positive entry. |
| 8 | **Unbounded memory** from buffering (G2 requires buffering). | `io.LimitReader` on the upstream body + `max_body_bytes` cap; oversized responses are served but not cached. |

---

## 7. Local setup

The default path requires **nothing but Go and the internet**:

```bash
make run        # memory backend, :8080, no Docker, no Redis
make test       # full suite with -race, fake upstream, zero network
make lint
```

Opt into the multi-instance demonstration:

```bash
make docker-up  # 2 app replicas + Redis + nginx LB, CACHE_BACKEND=redis
make prove      # burst 200 concurrent requests, print upstream call count
```

`docker-compose` exists specifically to demonstrate G3: two replicas, one upstream
call. The Dockerfile is multi-stage → distroless, static binary, non-root, healthcheck.

---

## 8. Test and proof plan

The bonus requirement is a concurrency claim, so it needs a concurrency proof, not
an assertion in a README.

| Test | Asserts |
|---|---|
| **`TestSingleFlight_ConcurrentColdKey`** | 200 goroutines hit a cold key against a fake upstream holding an atomic counter → **counter == 1**, 200 successful responses, 1 `X-Cache: MISS` and 199 `HIT`. This is the direct proof of the bonus. |
| **`TestOrdering_CacheVisibleBeforeFlush`** | Fake upstream; the instant the first response's body is readable, fire a second request → must be a `HIT`. This is the G2 race, made deterministic. |
| **`TestLeaderCancellation`** | Leader's request context cancelled mid-fetch → followers still succeed, upstream called once. |
| **`TestNegativeCaching`** | Two requests for a 404 ID → one upstream call; 404 partition never evicts 2xx entries. |
| **`TestValidationCostsNothing`** | `/posts/abc`, `/posts/0`, `/posts?x=1` → `400`, upstream counter stays `0`. |
| **`TestPolicyDisabled`** | Route with `enabled: false` → every request reaches upstream. |
| **`TestMiddlewareIsGeneric`** | Middleware exercised against a fake handler with no knowledge of posts/todos. |
| **LRU / TTL unit tests** | Eviction order, expiry, byte bounds, concurrent access under `-race`. |
| **Redis path** | `miniredis` for lock acquire/release/expiry and two-instance coordination — no live Redis needed in CI. |

Everything runs under `-race`. No test touches the real upstream.

---

## 9. Build order

1. **Foundation** — config, apperror, model, structured logging, graceful shutdown.
2. **Upstream client** — timeouts, bounded retry with jitter, size limits + tests.
3. **Cache layer** — `Cache` interface, memory LRU/TTL with negative partition + tests.
4. **The middleware** — recorder, key builder, policy, single-flight, G2 ordering
   + the concurrency proof tests. *This is the centre of gravity; most care goes here.*
5. **Transport** — router, handlers, param validation, error envelope + tests.
6. **Redis + lock** — L2 backend, `SET NX` lock, follower polling + miniredis tests.
7. **Ops** — Dockerfile, docker-compose (2 replicas + Redis + LB), Makefile, README.

Phases 1–5 deliver a complete, correct, single-instance service that satisfies every
basic requirement and the bonus. Phase 6 extends the bonus guarantee across replicas.
Phase 7 packages it.

---

## 10. Explicitly out of scope

Named so the omissions read as decisions rather than gaps:

- **Prometheus / `/metrics`** — counters exist internally and appear in logs
  (`cache=hit|miss`, upstream call count). Not exposed; would be the first addition.
- **Cache invalidation / purge API** — data is immutable; TTL is the only lifecycle.
- **Stale-while-revalidate** — pointless for immutable data.
- **`POST`/`PUT`/`DELETE` proxying** — not in the requirements, and the middleware
  correctly declines to cache non-`GET` anyway.
- **Auth** — no requirement stated; rate limiting is keyed on client IP.
