// Package notification is the notification service.
//
// It has no OpenAPI spec: in the slice this service is consumer-only, driven by BookingConfirmed
// from contracts/asyncapi/booking-events.yaml. It still serves /healthz and /readyz for compose
// and orchestration probes, per the service-template skill.
//
// The AsyncAPI contract is the source of truth; this code never defines it. See CLAUDE.md in
// this directory for scope, ownership, and events.
//
// Intentionally empty: service logic is written by the notification service agent (M2), not here.
package notification
