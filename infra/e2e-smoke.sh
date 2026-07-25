#!/usr/bin/env bash
# End-to-end proof of the booking slice: browse -> hold -> pay -> confirm -> email,
# threading ONE correlation id through the whole flow. Run from the repo root.
#
# HTTP goes to host-published ports (catalog 8081, availability 8082, booking 8083,
# payment 8084, mailhog 8025). psql seed/verify runs inside the postgres container.
set -uo pipefail

CID="e2e-$(date +%s)"                       # one correlation id for the whole journey
CUSTOMER="01920000-0000-7000-8000-0000000000c1"
PRODUCT="01920000-0000-7000-8000-0000000000d1"
RESOURCE="01920000-0000-7000-8000-0000000000e1"
WHSEC="whsec_dev_secret"                     # matches compose default STRIPE_WEBHOOK_SECRET

psql() { sg docker -c "docker compose exec -T postgres psql -U booking -d $1 -v ON_ERROR_STOP=1 -qtA -c \"$2\""; }
say()  { printf '\n=== %s ===\n' "$1"; }

# Forge a gateway-style bearer token: booking/payment read `sub` from an UNVERIFIED JWT (ADR-0006,
# the gateway verifies in prod). header.payload.sig — only payload matters.
BEARER="$(python3 - "$CUSTOMER" <<'PY'
import base64, json, sys
def b64(d): return base64.urlsafe_b64encode(d).decode().rstrip("=")
sub=sys.argv[1]
h=b64(b'{"alg":"none","typ":"JWT"}'); p=b64(json.dumps({"sub":sub}).encode())
print(f"{h}.{p}.sig")
PY
)"

say "1. SEED catalog product (browse has something to find)"
psql catalog "INSERT INTO product (id,resource_id,type,name,description,capacity,location,base_price_minor,currency,media_urls) VALUES ('$PRODUCT','$RESOURCE','yacht','E2E Test Yacht','A boat for the e2e',8,'lake-geneva',250000,'EUR','{}') ON CONFLICT (id) DO NOTHING;" && echo "seeded product $PRODUCT"

say "2. BROWSE — GET /v1/products (catalog:8081)"
curl -sS -H "X-Correlation-Id: $CID" "http://localhost:8081/v1/products?location=lake-geneva" | python3 -m json.tool | head -30

say "3. HOLD — POST /v1/bookings (booking:8083)"
STARTS="2026-09-01T10:00:00Z"; ENDS="2026-09-01T12:00:00Z"
HOLD=$(curl -sS -X POST "http://localhost:8083/v1/bookings" \
  -H "Authorization: Bearer $BEARER" -H "X-Correlation-Id: $CID" \
  -H "Idempotency-Key: hold-$CID" -H "Content-Type: application/json" \
  -d "{\"product_id\":\"$PRODUCT\",\"starts_at\":\"$STARTS\",\"ends_at\":\"$ENDS\",\"party_size\":2}")
echo "$HOLD" | python3 -m json.tool
BOOKING_ID=$(echo "$HOLD" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
echo "booking_id=$BOOKING_ID"
[ -n "$BOOKING_ID" ] || { echo "HOLD FAILED — stopping"; exit 1; }

say "4. WAIT for BookingHeld -> payment (booking_context) to propagate"
for i in $(seq 1 15); do
  ctx=$(psql payment "SELECT 1 FROM booking_context WHERE booking_id='$BOOKING_ID' LIMIT 1;" 2>/dev/null)
  [ "$ctx" = "1" ] && { echo "payment has booking_context after ${i}s"; break; }
  sleep 1
done

say "5. PAY — POST /v1/payments (payment:8084)"
PAY=$(curl -sS -X POST "http://localhost:8084/v1/payments" \
  -H "Authorization: Bearer $BEARER" -H "X-Correlation-Id: $CID" \
  -H "Idempotency-Key: pay-$CID" -H "Content-Type: application/json" \
  -d "{\"booking_id\":\"$BOOKING_ID\"}")
echo "$PAY" | python3 -m json.tool
PI=$(psql payment "SELECT provider_payment_id FROM payment WHERE booking_id='$BOOKING_ID';" 2>/dev/null)
echo "provider_payment_id=$PI"
[ -n "$PI" ] || { echo "PAY FAILED (no PaymentIntent) — stopping"; exit 1; }

say "6. WEBHOOK — POST signed payment_intent.succeeded (payment:8084)"
BODY="{\"id\":\"evt_$CID\",\"type\":\"payment_intent.succeeded\",\"data\":{\"object\":{\"id\":\"$PI\"}}}"
SIG=$(python3 - "$WHSEC" "$BODY" <<'PY'
import hashlib, hmac, sys, time
secret, body = sys.argv[1], sys.argv[2]
t=int(time.time()); mac=hmac.new(secret.encode(), f"{t}.{body}".encode(), hashlib.sha256).hexdigest()
print(f"t={t},v1={mac}")
PY
)
curl -sS -X POST "http://localhost:8084/v1/webhooks/stripe" \
  -H "Stripe-Signature: $SIG" -H "Content-Type: application/json" -d "$BODY" -w "\n[webhook HTTP %{http_code}]\n"

say "7. WAIT for PaymentSucceeded -> booking confirm -> BookingConfirmed -> notification email"
for i in $(seq 1 20); do
  st=$(psql booking "SELECT status FROM booking WHERE id='$BOOKING_ID';" 2>/dev/null)
  cnt=$(curl -sS "http://localhost:8025/api/v2/messages" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("total",0))' 2>/dev/null)
  echo "  t+${i}s: booking.status=$st  mailhog.total=$cnt"
  { [ "$st" = "confirmed" ] && [ "${cnt:-0}" -ge 1 ]; } && break
  sleep 1
done

say "8. VERIFY email in Mailhog + correlation id in the reservation"
curl -sS "http://localhost:8025/api/v2/messages" | python3 -c '
import sys,json
d=json.load(sys.stdin); items=d.get("items",[])
print("emails:", d.get("total",0))
for m in items[:2]:
    h=m.get("Content",{}).get("Headers",{})
    print("  To:", h.get("To"), "| Subject:", h.get("Subject"))
'
say "RESULT — booking status + payment status"
echo "booking:      $(psql booking "SELECT status FROM booking WHERE id='$BOOKING_ID';")"
echo "payment:      $(psql payment "SELECT status FROM payment WHERE booking_id='$BOOKING_ID';")"
echo "reservation:  $(psql availability "SELECT status FROM reservation WHERE booking_id='$BOOKING_ID';")"
echo "correlation-id used: $CID"
