//go:build integration

// Saga integration tests against a real Postgres (AC3, AC4).
//
// The happy path (hold→pay→confirm) and both compensation paths (payment-failed, TTL-release) are
// each driven end to end through the real store and the real consumer handlers, and the emitted
// envelope is validated against the AsyncAPI spec. Availability and Catalog are faked in-process:
// those services are not part of this slice, so the confirm HTTP call and the product lookup are
// stubbed rather than hit for real. Flagged in the PR.
package events

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/morisempai/wakewake/shared/contracts/events"
	"github.com/morisempai/wakewake/shared/platform/inbox"
	"github.com/morisempai/wakewake/shared/testkit/eventtest"
	"github.com/morisempai/wakewake/shared/testkit/pgtest"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
	"github.com/morisempai/wakewake/services/booking/internal/infra"
)

const (
	consumerName = "booking"
	holdTTL      = 15 * time.Minute
)

func newID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return id.String()
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeReservations stands in for Availability. Confirm's result is settable so a test can force the
// already-released branch.
type fakeReservations struct {
	mu         sync.Mutex
	confirmErr error
	confirmN   int
}

func (f *fakeReservations) Hold(_ context.Context, req domain.HoldRequest) (domain.Reservation, error) {
	return domain.Reservation{ID: newIDErr(), BookingID: req.BookingID, ExpiresAt: time.Now().UTC().Add(holdTTL)}, nil
}

func (f *fakeReservations) Confirm(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmN++
	return f.confirmErr
}

func newIDErr() string {
	id, _ := uuid.NewV7()
	return id.String()
}

type fakeCatalog struct{ product domain.Product }

func (f *fakeCatalog) Product(context.Context, string) (domain.Product, error) { return f.product, nil }

type fixture struct {
	store               *infra.Store
	svc                 *domain.Service
	pool                *pgxpool.Pool
	reservations        *fakeReservations
	paymentSucceeded    *PaymentSucceededHandler
	paymentFailed       *PaymentFailedHandler
	reservationReleased *ReservationReleasedHandler
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolving migrations: %v", err)
	}
	pool := pgtest.Postgres(t, dir)

	store := infra.NewStore(pool, nil)
	res := &fakeReservations{}
	cat := &fakeCatalog{product: domain.Product{ResourceID: newID(t), Capacity: 8, PriceMinor: 15000, Currency: "EUR"}}
	svc := domain.NewService(store, res, cat, time.Now, func() (string, error) { return newID(t), nil }, holdTTL)

	log := discardLogger()
	return &fixture{
		store:               store,
		svc:                 svc,
		pool:                pool,
		reservations:        res,
		paymentSucceeded:    NewPaymentSucceededHandler(store, svc, log),
		paymentFailed:       NewPaymentFailedHandler(store, svc, log),
		reservationReleased: NewReservationReleasedHandler(store, svc, log),
	}
}

// hold creates a held booking through the real CreateBooking path.
func (f *fixture) hold(t *testing.T) domain.Booking {
	t.Helper()
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Hour)
	b, err := f.svc.CreateBooking(context.Background(), domain.CreateCommand{
		CustomerID:  newID(t),
		ProductID:   newID(t),
		StartsAt:    start,
		EndsAt:      start.Add(time.Hour),
		PartySize:   2,
		Idempotency: domain.Claim{Key: "idem-" + newID(t), Fingerprint: "fp-" + newID(t)},
	})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	return b
}

func (f *fixture) process(t *testing.T, raw []byte, h inbox.Handler) (bool, error) {
	t.Helper()
	env, err := events.Parse(raw)
	if err != nil {
		t.Fatalf("parsing envelope: %v", err)
	}
	return inbox.Process(context.Background(), f.pool, consumerName, env, h)
}

func (f *fixture) reload(t *testing.T, id string) domain.Booking {
	t.Helper()
	b, err := f.store.ByID(context.Background(), id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return b
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func outboxPayload(t *testing.T, pool *pgxpool.Pool, event string) []byte {
	t.Helper()
	var payload []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT payload FROM outbox WHERE event = $1`, event).Scan(&payload); err != nil {
		t.Fatalf("reading %s from outbox: %v", event, err)
	}
	return payload
}

func paymentSucceeded(t *testing.T, bookingID string) []byte {
	t.Helper()
	return eventtest.Envelope(t, events.PaymentSucceeded, events.PaymentSucceededPayload{
		PaymentID: newID(t), BookingID: bookingID, AmountMinor: 15000, Currency: "EUR", ProviderPaymentID: "pi_123",
	}, "corr-"+newID(t))
}

func paymentFailed(t *testing.T, bookingID string) []byte {
	t.Helper()
	return eventtest.Envelope(t, events.PaymentFailed, events.PaymentFailedPayload{
		PaymentID: newID(t), BookingID: bookingID, AmountMinor: 15000, Currency: "EUR", FailureCode: "card_declined",
	}, "corr-"+newID(t))
}

func reservationReleased(t *testing.T, bookingID string, reason events.ReleaseReason) []byte {
	t.Helper()
	return eventtest.Envelope(t, events.ReservationReleased, events.ReservationReleasedPayload{
		ReservationID: newID(t), ResourceID: newID(t), BookingID: bookingID,
		StartsAt: time.Now().UTC(), EndsAt: time.Now().UTC().Add(time.Hour), Reason: reason,
	}, "corr-"+newID(t))
}

// ---------------------------------------------------------------------------
// AC4 — the happy path: hold → pay → confirm
// ---------------------------------------------------------------------------

func TestHappyPathConfirmsAndEmitsBookingConfirmed_Issue14_AC4(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)

	// The hold already staged BookingHeld; it must validate against the contract.
	eventtest.AssertValidAgainstSpec(t, events.BookingHeld, outboxPayload(t, f.pool, events.BookingHeld))

	processed, err := f.process(t, paymentSucceeded(t, held.ID), f.paymentSucceeded.Handle)
	if err != nil {
		t.Fatalf("processing PaymentSucceeded: %v", err)
	}
	if !processed {
		t.Fatal("first delivery reported as a duplicate")
	}
	if f.reservations.confirmN != 1 {
		t.Errorf("Availability.Confirm called %d times, want 1 (confirm is synchronous)", f.reservations.confirmN)
	}

	got := f.reload(t, held.ID)
	if got.Status != domain.StatusConfirmed {
		t.Errorf("status = %q, want confirmed", got.Status)
	}
	if got.HoldExpiresAt != nil {
		t.Error("hold_expires_at should be cleared once confirmed")
	}

	payload := outboxPayload(t, f.pool, events.BookingConfirmed)
	eventtest.AssertValidAgainstSpec(t, events.BookingConfirmed, payload)
	var decoded events.BookingConfirmedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding BookingConfirmed: %v", err)
	}
	if decoded.BookingID != held.ID || decoded.PaymentID == "" {
		t.Errorf("BookingConfirmed payload wrong: %+v", decoded)
	}
}

// ---------------------------------------------------------------------------
// AC4 — compensation: payment failed
// ---------------------------------------------------------------------------

func TestPaymentFailedCancelsAndEmitsBookingCancelled_Issue14_AC4(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)

	if _, err := f.process(t, paymentFailed(t, held.ID), f.paymentFailed.Handle); err != nil {
		t.Fatalf("processing PaymentFailed: %v", err)
	}

	got := f.reload(t, held.ID)
	if got.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}

	payload := outboxPayload(t, f.pool, events.BookingCancelled)
	eventtest.AssertValidAgainstSpec(t, events.BookingCancelled, payload)
	var decoded events.BookingCancelledPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if decoded.Reason != events.CancelPaymentFailed || decoded.PreviousStatus != events.BookingStatusHeld {
		t.Errorf("BookingCancelled payload wrong: %+v", decoded)
	}
	// Compensation is event-driven: booking must not have called Availability's confirm/release.
	if f.reservations.confirmN != 0 {
		t.Error("payment-failure compensation called Availability over HTTP; it must be event-driven")
	}
}

// ---------------------------------------------------------------------------
// AC4 — compensation: TTL release
// ---------------------------------------------------------------------------

func TestTTLReleaseCancelsTheExpiredHold_Issue14_AC4(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)

	if _, err := f.process(t, reservationReleased(t, held.ID, events.ReleaseHoldExpired), f.reservationReleased.Handle); err != nil {
		t.Fatalf("processing ReservationReleased: %v", err)
	}

	got := f.reload(t, held.ID)
	if got.Status != domain.StatusCancelled {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if got.CancelReason == nil || *got.CancelReason != domain.ReasonHoldExpired {
		t.Errorf("cancel_reason = %v, want hold_expired", got.CancelReason)
	}
	eventtest.AssertValidAgainstSpec(t, events.BookingCancelled, outboxPayload(t, f.pool, events.BookingCancelled))
}

// A release booking itself caused (payment_failed / booking_cancelled) must be ignored, or the
// consumer would cancel a booking a second time.
func TestReservationReleasedIgnoresReasonsBookingCaused_Issue14_AC4(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)

	processed, err := f.process(t, reservationReleased(t, held.ID, events.ReleaseBookingCancelled), f.reservationReleased.Handle)
	if err != nil {
		t.Fatalf("processing: %v", err)
	}
	if !processed {
		t.Error("the event was not marked processed")
	}
	if got := f.reload(t, held.ID); got.Status != domain.StatusHeld {
		t.Errorf("status = %q, want held — a booking-caused release must not cancel the booking", got.Status)
	}
	// Only the BookingHeld from the hold; no BookingCancelled.
	if n := countRows(t, f.pool, "outbox"); n != 1 {
		t.Errorf("outbox holds %d rows, want 1 — the ignored release must stage nothing", n)
	}
}

// ---------------------------------------------------------------------------
// AC3 — consumers are idempotent and ignore unknown fields
// ---------------------------------------------------------------------------

func TestDuplicateDeliveryConfirmsOnlyOnce_Issue14_AC3(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)
	raw := paymentSucceeded(t, held.ID)

	if _, err := f.process(t, raw, f.paymentSucceeded.Handle); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	processed, err := f.process(t, raw, f.paymentSucceeded.Handle)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if processed {
		t.Error("the redelivery ran the handler again; it must be suppressed by the envelope id")
	}

	// One BookingHeld + one BookingConfirmed, not two confirmations.
	if n := countRows(t, f.pool, "outbox"); n != 2 {
		t.Errorf("outbox holds %d rows after a redelivery, want 2", n)
	}
	if n := countRows(t, f.pool, "processed_event"); n != 1 {
		t.Errorf("processed_event holds %d rows, want 1", n)
	}
	if f.reservations.confirmN != 1 {
		t.Errorf("Availability.Confirm called %d times across a redelivery, want 1", f.reservations.confirmN)
	}
}

func TestUnknownPayloadFieldsAreIgnored_Issue14_AC3(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)

	raw := eventtest.WithUnknownField(t, paymentSucceeded(t, held.ID), "settlement_batch", "sb_987")
	if _, err := f.process(t, raw, f.paymentSucceeded.Handle); err != nil {
		t.Fatalf("an unknown payload field was rejected: %v", err)
	}
	if got := f.reload(t, held.ID); got.Status != domain.StatusConfirmed {
		t.Errorf("status = %q, want confirmed — the event should have been handled normally", got.Status)
	}
}

func TestPaymentFailedIgnoresUnknownFieldsAndIsIdempotent_Issue14_AC3(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	held := f.hold(t)
	raw := eventtest.WithUnknownField(t, paymentFailed(t, held.ID), "decline_network", "visa")

	if _, err := f.process(t, raw, f.paymentFailed.Handle); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	processed, err := f.process(t, raw, f.paymentFailed.Handle)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if processed {
		t.Error("the redelivery ran the handler again")
	}
	if n := countRows(t, f.pool, "outbox"); n != 2 {
		t.Errorf("outbox holds %d rows, want 2 (held + one cancelled)", n)
	}
}

// A payment for a booking this service has never seen is accepted, not dead-lettered forever.
func TestPaymentForAnUnknownBookingIsAccepted_Issue14_AC3(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	processed, err := f.process(t, paymentSucceeded(t, newID(t)), f.paymentSucceeded.Handle)
	if err != nil {
		t.Fatalf("handling a payment for an unknown booking returned %v, want nil", err)
	}
	if !processed {
		t.Error("the event was not processed")
	}
}
