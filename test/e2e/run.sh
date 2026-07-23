#!/usr/bin/env bash
# E2E gate: run the plugin under Yaegi inside real traefik.
# Asserts: (1) main starts despite a dead endpoint; (2) the live leaf's
# router is aggregated with the plugin's naming; (3) a request through
# main two-hops to the leaf's backend; (4) provider provenance.
set -euo pipefail
cd "$(dirname "$0")"

cleanup() { docker compose down -t 2 >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker compose up -d --quiet-pull

fail() { echo "FAIL: $1"; docker compose logs main | tail -30; exit 1; }

# (1) main must come up and expose its API despite deadleaf.
for i in $(seq 1 30); do
  curl -sf -m 2 http://127.0.0.1:18081/api/version >/dev/null && break
  [ "$i" = 30 ] && fail "main traefik API never came up (startup crash?)"
  sleep 1
done

# (2) aggregated router appears (poll interval 2s; allow settle time).
routers=""
for i in $(seq 1 15); do
  raw=$(curl -s -m 2 http://127.0.0.1:18081/api/rawdata || true)
  routers=$(printf '%s' "$raw" | python3 -c 'import sys,json
try: print("\n".join(json.load(sys.stdin)["routers"].keys()))
except Exception: pass')
  echo "$routers" | grep -q '^whoami-leaf1@' && break
  [ "$i" = 15 ] && { echo "$routers"; fail "whoami-leaf1 router never aggregated"; }
  sleep 2
done
echo "routers seen:"; echo "$routers" | sed 's/^/  /'

# (3) two-hop end to end.
body=$(curl -sf -m 5 -H 'Host: whoami.test' http://127.0.0.1:18080/) || fail "request through main failed"
echo "$body" | grep -q 'Hostname:' || fail "whoami did not answer through the two-hop path"

# (4) provenance: router must come from the plugin provider namespace.
echo "$routers" | grep '^whoami-leaf1@' | grep -q '@plugin-leafprovider' || fail "router not in plugin provider namespace: $(echo "$routers" | grep '^whoami-leaf1@')"

# (5) main's log must show deadleaf isolated (logged, not fatal).
docker compose logs main 2>&1 | grep -q 'endpoint "deadleaf"' || fail "deadleaf error never logged (isolation path untested?)"

echo "E2E PASS"
