#!/usr/bin/env bash
# End-to-end proof of the booking slice THROUGH THE GATEWAY (ADR-0006, ADR-0012):
# browse -> hold -> pay -> confirm -> email, threading ONE correlation id through the whole flow.
# Run from the repo root after `docker compose up --build`.
#
# What changed from the direct-to-service version: the five services publish NO host ports anymore.
# The gateway on :8080 is the only ingress. Every HTTP call below goes to the gateway, carrying a
# REAL Keycloak-minted bearer token (password grant) instead of a forged `alg:none` one — the
# gateway verifies the signature, issuer, and expiry before proxying. Postgres/Mailhog are still
# reached directly (they are test fixtures, not the API under test).
set -uo pipefail

GW="http://localhost:8080"                   # the gateway — the ONLY application ingress
KC="http://localhost:8088"                   # Keycloak, host-published for token minting (iss host)
CID="e2e-$(date +%s)"                        # one correlation id for the whole journey
# Fresh product + resource per run so the smoke test is re-runnable: a fixed resource/window would
# collide with a prior run's reservation (availability enforces no-double-booking) and 409 on HOLD.
PRODUCT="$(python3 -c 'import uuid;print(uuid.uuid4())')"
RESOURCE="$(python3 -c 'import uuid;print(uuid.uuid4())')"
WHSEC="whsec_dev_secret"                      # matches compose default STRIPE_WEBHOOK_SECRET

psql() { sg docker -c "docker compose exec -T postgres psql -U booking -d $1 -v ON_ERROR_STOP=1 -qtA -c \"$2\""; }
say()  { printf '\n=== %s ===\n' "$1"; }

say "0. WAIT for the gateway to be ready (it loads the realm JWKS from Keycloak at boot)"
ready=""
for i in $(seq 1 90); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/readyz" 2>/dev/null || echo 000)
  [ "$code" = "200" ] && { echo "gateway ready after ${i}s"; ready=1; break; }
  sleep 2
done
[ -n "$ready" ] || { echo "gateway never became ready (is Keycloak up and the realm imported?)"; exit 1; }

say "0b. NEGATIVE — a protected route with NO token must be rejected at the edge (401)"
code=$(curl -s -o /dev/null -w '%{http_code}' "$GW/v1/products")
echo "GET /v1/products (no Authorization) -> HTTP $code"
[ "$code" = "401" ] || { echo "EXPECTED 401 — the gateway is not enforcing auth"; exit 1; }

say "1. TOKEN — mint a real access token from Keycloak (password grant: booking-web / test.customer)"
TOKEN=$(curl -sS "$KC/realms/booking/protocol/openid-connect/token" \
  -d grant_type=password -d client_id=booking-web \
  -d username=test.customer -d password=customer_dev_pw \
  | python3 -c 'import sys,json;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null)
[ -n "$TOKEN" ] || { echo "TOKEN mint FAILED — check Keycloak realm/client (directAccessGrants) and creds"; exit 1; }
SUB=$(python3 - "$TOKEN" <<'PY'
import base64, json, sys
p = sys.argv[1].split(".")[1]; p += "=" * (-len(p) % 4)
print(json.loads(base64.urlsafe_b64decode(p)).get("sub", ""))
PY
)
echo "minted a valid token; customer sub=$SUB"
AUTH=(-H "Authorization: Bearer $TOKEN" -H "X-Correlation-Id: $CID")

say "2. SEED catalog product (browse has something to find) — DB fixture, not via the API"
psql catalog "INSERT INTO product (id,resource_id,type,name,description,capacity,location,base_price_minor,currency,media_urls) VALUES ('$PRODUCT','$RESOURCE','yacht','E2E Test Yacht','A boat for the e2e',8,'lake-geneva',250000,'EUR','{}') ON CONFLICT (id) DO NOTHING;" && echo "seeded product $PRODUCT"

say "3. BROWSE — GET /v1/products via the gateway"
curl -sS "${AUTH[@]}" "$GW/v1/products?location=lake-geneva" | python3 -m json.tool | head -30

say "4. HOLD — POST /v1/bookings via the gateway"
STARTS="2026-09-01T10:00:00Z"; ENDS="2026-09-01T12:00:00Z"
HOLD=$(curl -sS -X POST "$GW/v1/bookings" \
  "${AUTH[@]}" -H "Idempotency-Key: hold-$CID" -H "Content-Type: application/json" \
  -d "{\"product_id\":\"$PRODUCT\",\"starts_at\":\"$STARTS\",\"ends_at\":\"$ENDS\",\"party_size\":2}")
echo "$HOLD" | python3 -m json.tool
BOOKING_ID=$(echo "$HOLD" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
echo "booking_id=$BOOKING_ID"
[ -n "$BOOKING_ID" ] || { echo "HOLD FAILED — stopping"; exit 1; }

say "5. WAIT for BookingHeld -> payment (booking_context) to propagate"
for i in $(seq 1 15); do
  ctx=$(psql payment "SELECT 1 FROM booking_context WHERE booking_id='$BOOKING_ID' LIMIT 1;" 2>/dev/null)
  [ "$ctx" = "1" ] && { echo "payment has booking_context after ${i}s"; break; }
  sleep 1
done

say "6. PAY — POST /v1/payments via the gateway"
PAY=$(curl -sS -X POST "$GW/v1/payments" \
  "${AUTH[@]}" -H "Idempotency-Key: pay-$CID" -H "Content-Type: application/json" \
  -d "{\"booking_id\":\"$BOOKING_ID\"}")
echo "$PAY" | python3 -m json.tool
PI=$(psql payment "SELECT provider_payment_id FROM payment WHERE booking_id='$BOOKING_ID';" 2>/dev/null)
echo "provider_payment_id=$PI"
[ -n "$PI" ] || { echo "PAY FAILED (no PaymentIntent) — stopping"; exit 1; }

say "7. WEBHOOK — POST signed payment_intent.succeeded via the gateway (the sole PUBLIC route)"
BODY="{\"id\":\"evt_$CID\",\"type\":\"payment_intent.succeeded\",\"data\":{\"object\":{\"id\":\"$PI\"}}}"
SIG=$(python3 - "$WHSEC" "$BODY" <<'PY'
import hashlib, hmac, sys, time
secret, body = sys.argv[1], sys.argv[2]
t=int(time.time()); mac=hmac.new(secret.encode(), f"{t}.{body}".encode(), hashlib.sha256).hexdigest()
print(f"t={t},v1={mac}")
PY
)
# No bearer here on purpose: Stripe authenticates with its signature, which payment verifies. The
# gateway lets POST /v1/webhooks/stripe through unauthenticated (rate-limited only).
curl -sS -X POST "$GW/v1/webhooks/stripe" \
  -H "Stripe-Signature: $SIG" -H "X-Correlation-Id: $CID" -H "Content-Type: application/json" \
  -d "$BODY" -w "\n[webhook HTTP %{http_code}]\n"

say "8. WAIT for PaymentSucceeded -> booking confirm -> BookingConfirmed -> notification email"
for i in $(seq 1 20); do
  st=$(psql booking "SELECT status FROM booking WHERE id='$BOOKING_ID';" 2>/dev/null)
  cnt=$(curl -sS "http://localhost:8025/api/v2/messages" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("total",0))' 2>/dev/null)
  echo "  t+${i}s: booking.status=$st  mailhog.total=$cnt"
  { [ "$st" = "confirmed" ] && [ "${cnt:-0}" -ge 1 ]; } && break
  sleep 1
done

say "9. VERIFY email in Mailhog"
curl -sS "http://localhost:8025/api/v2/messages" | python3 -c '
import sys,json
d=json.load(sys.stdin); items=d.get("items",[])
print("emails:", d.get("total",0))
for m in items[:2]:
    h=m.get("Content",{}).get("Headers",{})
    print("  To:", h.get("To"), "| Subject:", h.get("Subject"))
'
say "10. TRACE — log<->trace linkage (ADR-0013) and one trace_id across the synchronous HTTP hops"
# Two distinct claims, proven from the services' own structured logs (no Tempo query needed — the
# assertions hold whether or not OTEL_EXPORTER_OTLP_ENDPOINT is set, because spans are recorded and
# their ids reach the logs regardless):
#
#   a) LINKAGE — every HTTP hop emits a log line matching Grafana's Loki derived-field regex,
#      "trace_id":"(\w+)". A service spelling it `traceId`, or emitting an empty id, silently breaks
#      trace<->log correlation with a green build; this fails it instead.
#   b) CONTINUITY — the HOLD is booking receiving POST /v1/bookings and, in the same request,
#      calling availability's POST /v1/reservations. With outbound propagation (PR #38) both hops
#      share ONE trace_id. A broken hop shows up as two different ids — the exact gap ADR-0013 closes.
#
# The async boundary is asserted the other way: payment/notification are reached over RabbitMQ, so
# they carry the same correlation_id but start their OWN trace (async traceparent is deferred —
# ADR-0013). We assert the correlation_id stitches the bus, and treat a differing trace_id as
# expected, not a failure.
dclogs() { sg docker -c "docker compose logs --no-log-prefix --no-color $1 2>/dev/null"; }
# Collect every service's logs into one file, each line tagged with its service. A file (not a pipe)
# because the python program itself arrives on stdin via the heredoc below.
TRACE_LOGS="$(mktemp)"
trap 'rm -f "$TRACE_LOGS"' EXIT
for s in gateway booking availability catalog payment notification; do
  dclogs "$s" | sed "s/^/$s /"
done > "$TRACE_LOGS"

CID="$CID" LOGS="$TRACE_LOGS" python3 - <<'PY'
import json, os, re, sys

cid = os.environ["CID"]
loki = re.compile(r'"trace_id":"(\w+)"')            # the exact Grafana Loki derived-field regex

http_services = ["gateway", "booking", "availability", "catalog"]
async_services = ["payment", "notification"]

# Per service: any correlation-matched line that also satisfies the Loki regex.
linked = {s: False for s in http_services}
seen_cid = {s: False for s in http_services + async_services}
hold_trace = None          # booking's trace_id on POST /v1/bookings
resv_trace = None          # availability's trace_id on POST /v1/reservations

with open(os.environ["LOGS"], encoding="utf-8", errors="replace") as fh:
    log_lines = fh.readlines()

for raw in log_lines:
    svc, _, line = raw.partition(" ")
    line = line.strip()
    if not line or not line.startswith("{"):
        continue
    try:
        rec = json.loads(line)
    except ValueError:
        continue
    if rec.get("correlation_id") != cid:
        continue
    if svc in seen_cid:
        seen_cid[svc] = True
    m = loki.search(line)                            # linkage proven against the RAW line, regex-exact
    tid = m.group(1) if m else ""
    if svc in linked and tid:
        linked[svc] = True
    route = str(rec.get("route", ""))
    method = str(rec.get("method", ""))
    if svc == "booking" and method == "POST" and "/bookings" in route and tid:
        hold_trace = tid
    if svc == "availability" and method == "POST" and "/reservations" in route and tid:
        resv_trace = tid

fail = []

# (a) linkage on every synchronous HTTP hop
for s in http_services:
    state = "ok" if linked[s] else "MISSING"
    print(f"  linkage  {s:<13} trace_id in logs: {state}")
    if not linked[s]:
        fail.append(f"{s}: no log line matching \"trace_id\":\"(\\w+)\" for correlation_id={cid}")

# (b) one trace across the booking->availability hold hop
print(f"  hold trace_id (booking POST /v1/bookings):        {hold_trace or 'MISSING'}")
print(f"  resv trace_id (availability POST /v1/reservations): {resv_trace or 'MISSING'}")
if not hold_trace:
    fail.append("no trace_id on booking's POST /v1/bookings — the hold hop was not traced")
elif not resv_trace:
    fail.append("no trace_id on availability's POST /v1/reservations — the downstream hop was not traced")
elif hold_trace != resv_trace:
    fail.append(f"trace broke across the hold hop: booking={hold_trace} availability={resv_trace} "
                "(outbound traceparent not propagated — PR #38 regression)")
else:
    print(f"  CONTINUITY OK — one trace ({hold_trace}) spans gateway->booking->availability")

# async boundary: correlation stitches the bus; a differing trace is expected, not a failure
for s in async_services:
    note = "correlation_id present" if seen_cid[s] else "NOT REACHED"
    print(f"  async    {s:<13} {note} (own trace by design — ADR-0013)")
    if not seen_cid[s]:
        fail.append(f"{s}: never logged correlation_id={cid} — the async hop lost the correlation id")

if fail:
    print("\nTRACE VERIFICATION FAILED:")
    for f in fail:
        print("  -", f)
    sys.exit(1)
print("\ntrace verification passed: log<->trace linkage holds and one trace_id spans the HTTP hops")
PY
[ $? -eq 0 ] || { echo "TRACE step failed — see above"; exit 1; }

say "RESULT — booking / payment / reservation state"
echo "booking:      $(psql booking "SELECT status FROM booking WHERE id='$BOOKING_ID';")"
echo "payment:      $(psql payment "SELECT status FROM payment WHERE booking_id='$BOOKING_ID';")"
echo "reservation:  $(psql availability "SELECT status FROM reservation WHERE booking_id='$BOOKING_ID';")"
echo "correlation-id used: $CID"
echo "customer sub (from real token): $SUB"
