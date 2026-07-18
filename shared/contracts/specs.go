// Package contracts embeds the API and event specifications so Go code can validate itself
// against them at test time.
//
// The specs are a COPY of the repo-root contracts/ directory, synced by `make contracts-gen`.
// The copy exists because go:embed cannot reach outside its module root, and because a Docker
// build stage that copies only shared/ must still be able to build. A CI drift guard fails if
// the copy diverges from the source, so the duplication cannot rot silently.
//
// contracts/ at the repo root remains the source of truth. Never edit these copies.
package contracts

import "embed"

//go:embed specs
var Specs embed.FS

// Paths within Specs.
const (
	AsyncAPIPath = "specs/asyncapi/booking-events.yaml"

	OpenAPICatalog      = "specs/openapi/catalog.yaml"
	OpenAPIAvailability = "specs/openapi/availability.yaml"
	OpenAPIBooking      = "specs/openapi/booking.yaml"
	OpenAPIPayment      = "specs/openapi/payment.yaml"
)

// OpenAPIPathFor returns the embedded spec path for a service, and whether it has one.
// Notification has no HTTP contract — it is consumer-only in the slice.
func OpenAPIPathFor(service string) (string, bool) {
	p, ok := map[string]string{
		"catalog":      OpenAPICatalog,
		"availability": OpenAPIAvailability,
		"booking":      OpenAPIBooking,
		"payment":      OpenAPIPayment,
	}[service]
	return p, ok
}
