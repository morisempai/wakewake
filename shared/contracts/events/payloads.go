package events

import "time"

// Payload structs for the seven contract events.
//
// These are hand-written rather than generated (ADR-0009): no mature Go AsyncAPI generator
// exists, and the ones that do flatten the `allOf: [Envelope, {payload}]` composition, which is
// exactly wrong here. The guarantee that generation would provide — "these cannot drift from the
// spec" — is instead provided directly by the contract tests in this package, which validate
// every struct against the embedded AsyncAPI schema and fail on any field added or removed on
// either side.
//
// Money is an integer count of the currency's minor unit. Never a float: 0.1 + 0.2 != 0.3 in
// binary floating point, and this is a payments system.

// BookingHeldPayload — booking → payment. The slot is held and awaiting payment.
type BookingHeldPayload struct {
	BookingID     string    `json:"booking_id"`
	CustomerID    string    `json:"customer_id"`
	ProductID     string    `json:"product_id"`
	ResourceID    string    `json:"resource_id"`
	ReservationID string    `json:"reservation_id"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`

	// HoldExpiresAt is when the sweeper releases this hold unless payment confirms it first.
	HoldExpiresAt time.Time `json:"hold_expires_at"`

	TotalMinor int64  `json:"total_minor"`
	Currency   string `json:"currency"`
}

// BookingConfirmedPayload — booking → notification.
//
// Deliberately carries NO customer email or name. Notification resolves the recipient from
// customer_id at send time, so PII does not sit in the broker or in queue backlogs where it
// would outlive a deletion request (NFR-4).
type BookingConfirmedPayload struct {
	BookingID     string    `json:"booking_id"`
	CustomerID    string    `json:"customer_id"`
	ProductID     string    `json:"product_id"`
	ResourceID    string    `json:"resource_id"`
	ReservationID string    `json:"reservation_id"`
	PaymentID     string    `json:"payment_id"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	TotalMinor    int64     `json:"total_minor"`
	Currency      string    `json:"currency"`
}

// BookingCancelledPayload — booking → availability (compensation).
type BookingCancelledPayload struct {
	BookingID  string `json:"booking_id"`
	CustomerID string `json:"customer_id"`
	ResourceID string `json:"resource_id"`

	// ReservationID is a pointer because the contract types it `oneOf: [Uuid, null]` — null
	// means the hold was already released. An empty string would silently collapse "no
	// reservation" into "reservation with an unset id", and availability's compensation handler
	// needs to tell those apart.
	ReservationID *string `json:"reservation_id"`

	PreviousStatus BookingStatus `json:"previous_status"`
	Reason         CancelReason  `json:"reason"`
}

// PaymentSucceededPayload — payment → booking.
type PaymentSucceededPayload struct {
	PaymentID   string `json:"payment_id"`
	BookingID   string `json:"booking_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`

	// ProviderPaymentID is a Stripe PaymentIntent id — a reference, never an instrument.
	ProviderPaymentID string `json:"provider_payment_id"`
}

// PaymentFailedPayload — payment → booking.
type PaymentFailedPayload struct {
	PaymentID   string `json:"payment_id"`
	BookingID   string `json:"booking_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`

	// FailureCode is Stripe's decline code, e.g. card_declined, expired_card.
	FailureCode string `json:"failure_code"`
}

// ReservationCreatedPayload — availability → (no consumer in the slice).
type ReservationCreatedPayload struct {
	ReservationID string    `json:"reservation_id"`
	ResourceID    string    `json:"resource_id"`
	BookingID     string    `json:"booking_id"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// ReservationReleasedPayload — availability → booking.
//
// Booking reacts to exactly one reason: hold_expired, the TTL sweeper, which means an abandoned
// checkout must be cancelled. The other two reasons are consequences of booking's own actions
// and reacting to them would loop.
type ReservationReleasedPayload struct {
	ReservationID string        `json:"reservation_id"`
	ResourceID    string        `json:"resource_id"`
	BookingID     string        `json:"booking_id"`
	StartsAt      time.Time     `json:"starts_at"`
	EndsAt        time.Time     `json:"ends_at"`
	Reason        ReleaseReason `json:"reason"`
}

// BookingStatus is the booking state a cancellation moved away from.
type BookingStatus string

const (
	BookingStatusHeld      BookingStatus = "held"
	BookingStatusConfirmed BookingStatus = "confirmed"
)

// CancelReason is why a booking was cancelled.
type CancelReason string

const (
	CancelPaymentFailed     CancelReason = "payment_failed"
	CancelHoldExpired       CancelReason = "hold_expired"
	CancelCustomerCancelled CancelReason = "customer_cancelled"
	CancelOperatorCancelled CancelReason = "operator_cancelled"
)

// ReleaseReason is why a reservation was released. Note this is a different, smaller set than
// CancelReason: a reservation has no notion of who asked.
type ReleaseReason string

const (
	ReleasePaymentFailed    ReleaseReason = "payment_failed"
	ReleaseBookingCancelled ReleaseReason = "booking_cancelled"
	ReleaseHoldExpired      ReleaseReason = "hold_expired"
)
