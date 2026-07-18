package domain

import "time"

// STUB — signatures only, so the acceptance tests fail on assertions rather than on a compile
// error. Replaced by the real implementation in the commit that turns them green.

type Status string

const (
	StatusHeld      Status = "held"
	StatusConfirmed Status = "confirmed"
	StatusReleased  Status = "released"
)

type ReleaseReason string

const (
	ReasonPaymentFailed    ReleaseReason = "payment_failed"
	ReasonBookingCancelled ReleaseReason = "booking_cancelled"
	ReasonHoldExpired      ReleaseReason = "hold_expired"
)

func (r ReleaseReason) Valid() bool { return false }

type Window struct {
	StartsAt time.Time
	EndsAt   time.Time
}

type Emission struct {
	Event       string
	AggregateID string
	Payload     any
}

type Reservation struct {
	ID             string
	ResourceID     string
	BookingID      string
	StartsAt       time.Time
	EndsAt         time.Time
	Status         Status
	ExpiresAt      *time.Time
	ReleasedReason *ReleaseReason
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func NewReservation(id, resourceID, bookingID string, startsAt, endsAt time.Time) (Reservation, error) {
	return Reservation{}, nil
}

func (r Reservation) Confirm(now time.Time) (Reservation, []Emission, error) { return r, nil, nil }

func (r Reservation) Release(reason ReleaseReason, now time.Time) (Reservation, []Emission, error) {
	return r, nil, nil
}

func (r Reservation) ReleaseIfExpired(now time.Time) (Reservation, []Emission, error) {
	return r, nil, nil
}

func (r Reservation) CreatedEmission() Emission { return Emission{} }
