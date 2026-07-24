#!/usr/bin/env bash
# E2E gate: run the plugin under Yaegi inside real traefik.
# Asserts: (1) main starts despite a dead endpoint; (2) the live leaf's
# router is aggregated with the plugin's naming; (3) a request through
# main two-hops to the leaf's backend; (4) provider provenance;
# (5) dead-endpoint isolation logging; (6)-(9) per-route middleware
# attachment (routeMiddlewares): applied, absent-by-default, rawdata
# reflects it, and an unmatched key is logged.
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
  echo "$routers" | grep -q '^whoami-leaf1@' && echo "$routers" | grep -q '^whoami2-leaf1@' && break
  [ "$i" = 15 ] && { echo "$routers"; fail "whoami-leaf1/whoami2-leaf1 routers never aggregated"; }
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

# (6) routeMiddlewares applied: whoami has an override -> testmw's
# response header must be present on the two-hop response.
hdrs=$(curl -si -m 5 -H 'Host: whoami.test' http://127.0.0.1:18080/) || fail "request through main (whoami) failed"
echo "$hdrs" | grep -qi '^X-E2e-Mw: 1' || fail "routeMiddlewares override not applied: missing X-E2E-MW header for whoami: $(echo "$hdrs" | head -20)"

# (7) no entry for whoami2 -> falls back to endpoint default, which is
# unset here, so the request must succeed with NO testmw header.
hdrs2=$(curl -si -m 5 -H 'Host: whoami2.test' http://127.0.0.1:18080/) || fail "request through main (whoami2) failed"
echo "$hdrs2" | grep -qi '^X-E2e-Mw:' && fail "whoami2 unexpectedly carries X-E2E-MW header (route-scoping broken): $(echo "$hdrs2" | head -20)"

# (8) rawdata reflects the per-route attachment directly.
raw2=$(curl -s -m 2 http://127.0.0.1:18081/api/rawdata || true)
python3 -c '
import sys, json
d = json.load(sys.stdin)
r = d["routers"]
def mws(name):
    return r.get(name, {}).get("middlewares") or []
ok = True
if "testmw@file" not in mws("whoami-leaf1@plugin-leafprovider"):
    print("whoami-leaf1 missing testmw@file:", mws("whoami-leaf1@plugin-leafprovider")); ok = False
if mws("whoami2-leaf1@plugin-leafprovider"):
    print("whoami2-leaf1 unexpectedly has middlewares:", mws("whoami2-leaf1@plugin-leafprovider")); ok = False
sys.exit(0 if ok else 1)
' <<<"$raw2" || fail "rawdata does not reflect per-route middleware attachment"

# (9) the deliberately-unmatched routeMiddlewares key ("ghost") must be
# logged as matching no leaf router.
docker compose logs main 2>&1 | grep 'routeMiddlewares keys matching no leaf router' | grep -q 'ghost' || fail "unmatched routeMiddlewares key 'ghost' never logged"

echo "E2E PASS"
