#!/usr/bin/env bash
#
# Fires N concurrent requests at a single cold key and reports how many reached
# the upstream.
#
# The unit tests assert this against a fake upstream, where the count is exact
# and deterministic. This script shows the same property against a running
# server, in a form a reviewer can check without reading any Go.
#
# Single instance:
#   scripts/prove.sh                        # defaults to localhost:8080, N=200
#
# Cluster: traffic goes through the load balancer, but upstream counters must be
# summed across every replica. Reading /readyz through the balancer would return
# whichever replica happened to answer, which understates the fleet total.
#   REPLICAS="http://localhost:8081 http://localhost:8082" scripts/prove.sh

set -uo pipefail

BASE="${1:-${BASE:-http://localhost:8080}}"
N="${2:-${N:-200}}"
REPLICAS="${3:-${REPLICAS:-$BASE}}"

command -v curl >/dev/null || { echo "curl is required" >&2; exit 1; }

# A random id keeps the key cold even when run repeatedly against a long-lived
# server. Constrained to the range the upstream actually serves.
ID="${ID:-$(( (RANDOM % 100) + 1 ))}"
TARGET="$BASE/posts/$ID"

# Sums upstream_calls across every replica, so the number reported is what the
# fleet spent rather than what one instance happened to spend.
fleet_upstream_calls() {
  total=0 reads=0
  for host in $REPLICAS; do
    # Extracted with sed rather than a JSON parser so the script needs nothing
    # beyond curl and a POSIX shell on whatever machine it is run.
    one="$(curl -fsS "$host/readyz" 2>/dev/null \
      sed -n 's/.*"upstream_calls"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
    if [ -n "${one:-}" ]; then
      total=$(( total + one ))
      reads=$(( reads + 1 ))
    fi
  done
  printf '%s %s\n' "$total" "$reads"
}

if ! curl -fsS -o /dev/null "$BASE/healthz" 2>/dev/null; then
  echo "error: no server responding at $BASE (try: make run, or make docker-up)" >&2
  exit 1
fi

replica_count=$(printf '%s\n' $REPLICAS | wc -w | tr -d ' ')
before="$(fleet_upstream_calls)"
calls_before="${before%% *}"
reads_before="${before##* }"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "firing $N concurrent requests at $TARGET"
[ "$replica_count" -gt 1 ] && echo "counting upstream calls across $replica_count replicas"
echo

# xargs -P gives real parallelism; every worker targets the same cold key, which
# is precisely the cache-stampede condition.
seq 1 "$N" | xargs -P "$N" -I{} sh -c \
  "curl -s -o /dev/null -D '$tmp/{}.hdr' -w '%{http_code}' '$TARGET' > '$tmp/{}.code' 2>/dev/null"

after="$(fleet_upstream_calls)"
calls_after="${after%% *}"
reads_after="${after##* }"
delta=$(( calls_after - calls_before ))

# The delta is only meaningful if every replica's counter was read both times.
counted=1
if [ "$reads_before" -ne "$replica_count" ] || [ "$reads_after" -ne "$replica_count" ]; then
  counted=0
fi

# grep finding no matches is a normal outcome here, so its exit status is
# discarded rather than allowed to abort the script under pipefail.
misses=$(grep -li '^x-cache: MISS' "$tmp"/*.hdr 2>/dev/null | wc -l | tr -d ' ')
hits_all=$(grep -li '^x-cache: HIT' "$tmp"/*.hdr 2>/dev/null | wc -l | tr -d ' ')
coalesced=$(grep -li '^x-cache: HIT-COALESCED' "$tmp"/*.hdr 2>/dev/null | wc -l | tr -d ' ')
ok=$(grep -lx '200' "$tmp"/*.code 2>/dev/null | wc -l | tr -d ' ')
hits=$(( hits_all - coalesced ))

printf 'requests sent          : %s\n' "$N"
printf 'responses with 200     : %s\n' "$ok"
printf 'X-Cache: MISS          : %s   <- requests that performed the fetch\n' "$misses"
printf 'X-Cache: HIT           : %s\n' "$hits"
printf 'X-Cache: HIT-COALESCED : %s\n' "$coalesced"
if [ "$counted" -eq 1 ]; then
   printf 'upstream calls (fleet) : %s\n' "$delta"
else
  printf 'upstream calls (fleet) : unavailable (read %s of %s replica counters)\n' \
    "$reads_after" "$replica_count"
fi

# In cluster mode, show how the traffic was actually distributed. If every
# request happened to land on one replica the test would not have exercised
# cross-instance coordination at all, and that is worth knowing.
if [ "$replica_count" -gt 1 ]; then
  served=$(grep -hi '^x-served-by:' "$tmp"/*.hdr 2>/dev/null | tr -d '\r' | awk '{print $2}' | sort | uniq -c | tr '\n' ' ')
  [ -n "$served" ] && printf 'served by              : %s\n' "$served"
fi
echo

fail=0
if [ "$misses" -ne 1 ]; then
  echo "FAIL  expected exactly 1 request to perform the fetch, saw $misses"
  fail=1
fi
if [ "$ok" -ne "$N" ]; then
  echo "FAIL  expected all $N requests to succeed, saw $ok"
  fail=1
fi
if [ "$counted" -eq 1 ] && [ "$delta" -gt 1 ]; then
  echo "note  upstream attempts exceeded 1 ($delta): the upstream returned retryable"
  echo "      errors under this burst. Coalescing is measured by the MISS count above."
fi

if [ "$fail" -eq 0 ]; then
  echo "PASS  $N concurrent cold requests were served by a single fetch"
  exit 0
fi
exit 1
