#!/usr/bin/env bash
# E2E gate: run the plugin under Yaegi inside real traefik.
# Asserts: (1) main starts despite a dead endpoint; (2) the live leaf's
# router is aggregated with the plugin's naming; (3) a request through
# main two-hops to the leaf's backend; (4) provider provenance;
# (5) dead-endpoint isolation logging; (6)-(9) per-route middleware
# attachment (routeMiddlewares): applied, absent-by-default, rawdata
# reflects it, and an unmatched key is logged; (10)-(13) TLS: a leaf API
# behind an mTLS gate aggregates, the same gate rejects an endpoint with
# no client keypair, the generated serversTransport reaches rawdata, and
# a TLS failure stays isolated.
set -euo pipefail
cd "$(dirname "$0")"

cleanup() { docker compose down -t 2 >/dev/null 2>&1 || true; }
trap cleanup EXIT

# Certificates for the mTLS endpoint. Generated rather than committed:
# no key material in the repository, and nothing to expire. RSA and a
# separate CSR step for portability across OpenSSL and LibreSSL.
gen_certs() {
  [ -f certs/client.crt ] && return 0
  echo "generating e2e certificates"
  mkdir -p certs
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout certs/ca.key -out certs/ca.pem -subj "/CN=leaf-provider-e2e-ca" 2>/dev/null

  openssl req -newkey rsa:2048 -nodes \
    -keyout certs/server.key -out certs/server.csr -subj "/CN=leafmtls" 2>/dev/null
  openssl x509 -req -in certs/server.csr -CA certs/ca.pem -CAkey certs/ca.key \
    -CAcreateserial -out certs/server.crt -days 2 \
    -extfile <(printf 'subjectAltName=DNS:leafmtls\nextendedKeyUsage=serverAuth\n') 2>/dev/null

  openssl req -newkey rsa:2048 -nodes \
    -keyout certs/client.key -out certs/client.csr -subj "/CN=main-instance" 2>/dev/null
  openssl x509 -req -in certs/client.csr -CA certs/ca.pem -CAkey certs/ca.key \
    -CAcreateserial -out certs/client.crt -days 2 \
    -extfile <(printf 'extendedKeyUsage=clientAuth\n') 2>/dev/null

  # Readable by the container users that mount them.
  chmod 644 certs/*.key certs/*.crt certs/*.pem
}
gen_certs

docker compose up -d --quiet-pull

fail() { echo "FAIL: $1"; docker compose logs main | tail -30; exit 1; }

# Log assertions must retry. A dead endpoint only logs after its
# requestTimeout elapses (2s here), and endpoints are polled
# sequentially, so a single grep races the poll loop and fails
# spuriously. Retry for ~30s before believing the line is absent.
wait_for_log() {
  local pattern="$1" extra="${2:-}" i
  for i in $(seq 1 30); do
    if [ -n "$extra" ]; then
      docker compose logs main 2>&1 | grep "$pattern" | grep -q "$extra" && return 0
    else
      docker compose logs main 2>&1 | grep -q "$pattern" && return 0
    fi
    sleep 1
  done
  return 1
}

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
wait_for_log 'endpoint "deadleaf"' || fail "deadleaf error never logged (isolation path untested?)"

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
wait_for_log 'routeMiddlewares keys matching no leaf router' 'ghost' || fail "unmatched routeMiddlewares key 'ghost' never logged"

# (10) THE TLS GATE. mtlsleaf polls the leaf API through nginx with
# ssl_verify_client on. Its routers can only appear if the plugin built a
# tls.Config from a private CA and a client keypair *under Yaegi* and
# completed the mTLS handshake. Compiled tests cannot prove this.
for i in $(seq 1 20); do
  raw3=$(curl -s -m 2 http://127.0.0.1:18081/api/rawdata || true)
  printf '%s' "$raw3" | grep -q 'whoami3-mtlsleaf@plugin-leafprovider' && break
  [ "$i" = 20 ] && {
    echo "--- leafmtls log ---"; docker compose logs leafmtls | tail -20
    fail "mtlsleaf never aggregated whoami3 (hop-1 mTLS broken under Yaegi?)"
  }
  sleep 2
done
echo "mTLS endpoint aggregated"

# (11) Negative control. mtlsnocert trusts the same CA but presents no
# client keypair, so the gate must reject it. If this endpoint DID
# aggregate, assertion (10) would prove nothing about client certs.
if printf '%s' "$raw3" | grep -q 'whoami3-mtlsnocert@plugin-leafprovider'; then
  fail "mtlsnocert aggregated without a client certificate — mTLS is not being enforced"
fi
wait_for_log 'endpoint "mtlsnocert"' || fail "mtlsnocert failure never logged (isolation path untested?)"
echo "mTLS negative control rejected as expected"

# (12) The generated serversTransport must be referenced by that
# endpoint's service AND actually resolve (hop 2).
#
# /api/rawdata does not expose serversTransports, so resolution is
# checked via service status instead. That is a real signal, not a
# proxy for one: a dangling reference gives status "disabled" with
# "servers transport not found <name>" (verified 2026-07-30). Traefik
# also rewrites the reference to its fully-qualified form, hence
# expecting "@plugin-leafprovider" here rather than the bare name the
# plugin emits.
python3 -c '
import sys, json
d = json.load(sys.stdin)
ok = True

svcs = d.get("services", {})
svc = svcs.get("mtlsleaf-leaf@plugin-leafprovider")
if svc is None:
    print("missing service mtlsleaf-leaf@plugin-leafprovider:", sorted(svcs)); ok = False
else:
    st = (svc.get("loadBalancer") or {}).get("serversTransport")
    if st != "mtlsleaf-transport@plugin-leafprovider":
        print("serversTransport =", repr(st), "want mtlsleaf-transport@plugin-leafprovider"); ok = False
    if svc.get("status") != "enabled":
        print("service not enabled -> generated transport did not resolve:",
              svc.get("status"), svc.get("error")); ok = False

# leaf1 has no tls block, so it must stay on traefiks global default.
plain = svcs.get("leaf1-leaf@plugin-leafprovider") or {}
if (plain.get("loadBalancer") or {}).get("serversTransport"):
    print("leaf1 unexpectedly pinned to a transport:", plain); ok = False

sys.exit(0 if ok else 1)
' <<<"$raw3" || fail "generated serversTransport missing, misreferenced, or unresolved"
echo "generated serversTransport referenced and resolved"

# (13) Everything above must not have disturbed the plain endpoints: the
# two-hop route still works with three TLS-configured siblings around it.
body2=$(curl -sf -m 5 -H 'Host: whoami.test' http://127.0.0.1:18080/) || fail "two-hop route broke after TLS endpoints were added"
echo "$body2" | grep -q 'Hostname:' || fail "whoami did not answer after TLS endpoints were added"

# And the mTLS endpoint's own route must two-hop for real, not merely
# appear in rawdata: main -> leaf2 -> whoami3, on a route learned
# entirely through the mTLS-gated poll.
body3=$(curl -sf -m 5 -H 'Host: whoami3.test' http://127.0.0.1:18080/) || fail "two-hop through the mTLS-learned route failed"
echo "$body3" | grep -q 'Hostname:' || fail "whoami3 did not answer through the mTLS-learned route"
echo "mTLS-learned route serves end to end"

echo "E2E PASS"
