package events

// Event names, which double as AMQP routing keys. These are contract values from
// contracts/asyncapi/booking-events.yaml — PascalCase past-tense facts, never commands.
const (
	BookingHeld         = "BookingHeld"
	BookingConfirmed    = "BookingConfirmed"
	BookingCancelled    = "BookingCancelled"
	PaymentSucceeded    = "PaymentSucceeded"
	PaymentFailed       = "PaymentFailed"
	ReservationCreated  = "ReservationCreated"
	ReservationReleased = "ReservationReleased"
)

// ExchangeName is the topic exchange every event is published to, per the AsyncAPI server
// binding. The routing key is the event name.
const ExchangeName = "booking.events"

// All is every event the contract defines, in spec order.
var All = []string{
	BookingHeld,
	BookingConfirmed,
	BookingCancelled,
	PaymentSucceeded,
	PaymentFailed,
	ReservationCreated,
	ReservationReleased,
}

// prototypes maps each event to a zero payload value. It is the single registry that keeps
// Validate, the contract tests, and testkit agreeing on what exists — adding an event to the
// contract without adding it here fails the drift test rather than silently going unvalidated.
var prototypes = map[string]func() any{
	BookingHeld:         func() any { return &BookingHeldPayload{} },
	BookingConfirmed:    func() any { return &BookingConfirmedPayload{} },
	BookingCancelled:    func() any { return &BookingCancelledPayload{} },
	PaymentSucceeded:    func() any { return &PaymentSucceededPayload{} },
	PaymentFailed:       func() any { return &PaymentFailedPayload{} },
	ReservationCreated:  func() any { return &ReservationCreatedPayload{} },
	ReservationReleased: func() any { return &ReservationReleasedPayload{} },
}

// IsKnown reports whether the event name appears in the contract.
func IsKnown(event string) bool {
	_, ok := prototypes[event]
	return ok
}

// Prototype returns a pointer to a zero payload for a known event, for reflective tests and
// generic decoding. The bool reports whether the event is known.
func Prototype(event string) (any, bool) {
	f, ok := prototypes[event]
	if !ok {
		return nil, false
	}
	return f(), true
}
