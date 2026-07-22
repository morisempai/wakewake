// Package api is the HTTP layer: it binds requests, calls one domain method, and maps the result
// onto the responses contracts/openapi/availability.yaml declares.
//
// It contains no business decisions. The types and the routing come from oapi-codegen against
// the spec (shared/contracts/openapi/availability), so a response shape that drifts from the
// contract is a compile error rather than something a contract test has to notice later.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/availability"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/idempotency"

	"github.com/morisempai/wakewake/services/availability/internal/domain"
)

// Idempotency-Key bounds from the spec's `minLength: 8, maxLength: 255`.
//
// Enforced here because oapi-codegen binds header parameters without applying string
// constraints. An 8-character floor is what stops "1" being sent as a key, which would collide
// across unrelated callers and replay one caller's reservation to another.
const (
	idempotencyKeyMinLen = 8
	idempotencyKeyMaxLen = 255
)

// defaultReleaseReason is used when POST .../actions/release arrives without a body.
//
// DEVIATION, flagged for a contract-change issue: the spec marks the request body
// `required: false` but makes `reason` required inside it, and says nothing about what an absent
// body means. Rejecting it would contradict `required: false`; inventing a fourth reason would
// contradict the enum. `booking_cancelled` is the closest honest reading — an unattributed
// external release is the compensation path — but it is a choice this code should not have had
// to make, since it lands in the audit trail and in the event Booking consumes.
const defaultReleaseReason = domain.ReasonBookingCancelled

// Server implements the generated strict server interface.
type Server struct {
	svc *domain.Service
	log *slog.Logger
}

// NewServer wires the HTTP layer.
func NewServer(svc *domain.Service, log *slog.Logger) *Server {
	return &Server{svc: svc, log: log}
}

var _ spec.StrictServerInterface = (*Server)(nil)

// CreateReservation reserves a window. Contract: 201 | 400 | 409 | 422.
func (s *Server) CreateReservation(ctx context.Context, request spec.CreateReservationRequestObject) (spec.CreateReservationResponseObject, error) {
	if request.Body == nil {
		return spec.CreateReservation400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ValidationFailed, "A JSON request body is required."))}, nil
	}

	if problem := validateIdempotencyKey(request.Params.IdempotencyKey); problem != "" {
		return spec.CreateReservation400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ValidationFailed, "The Idempotency-Key header is not usable.",
				detail("Idempotency-Key", problem)))}, nil
	}

	fingerprint, err := fingerprintOf(*request.Body)
	if err != nil {
		return nil, err
	}

	reservation, err := s.svc.Reserve(ctx, domain.ReserveCommand{
		ResourceID: request.Body.ResourceId.String(),
		BookingID:  request.Body.BookingId.String(),
		StartsAt:   request.Body.StartsAt,
		EndsAt:     request.Body.EndsAt,
		Idempotency: domain.Claim{
			Key:         request.Params.IdempotencyKey,
			Fingerprint: fingerprint,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidTimeWindow):
			// AC6. 422 rather than 400: the request is well-formed, it just asks for something
			// the domain forbids.
			return spec.CreateReservation422JSONResponse(errorBody(ctx, spec.InvalidTimeWindow,
				"ends_at must be strictly after starts_at.",
				detail("ends_at", "must be strictly after starts_at"))), nil

		case errors.Is(err, domain.ErrSlotUnavailable):
			// AC1. The exclusion constraint fired: another caller won this window. An expected
			// outcome of a lost race, so it is logged at info, not error.
			s.log.InfoContext(ctx, "reserve lost the race for the window",
				slog.String("resource_id", request.Body.ResourceId.String()),
				slog.String("booking_id", request.Body.BookingId.String()))
			return spec.CreateReservation409JSONResponse(errorBody(ctx, spec.ReservationOverlap,
				"The requested window is no longer available for this resource.")), nil

		case errors.Is(err, domain.ErrIdempotencyKeyReuse):
			// AC3.
			return spec.CreateReservation409JSONResponse(errorBody(ctx, spec.IdempotencyKeyReuse,
				"This Idempotency-Key was already used with a different request body.",
				detail("Idempotency-Key", "already used with a different request body"))), nil

		case errors.Is(err, domain.ErrMissingIdentifier):
			return spec.CreateReservation400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
				errorBody(ctx, spec.ValidationFailed, "resource_id and booking_id are required."))}, nil
		}
		return nil, err
	}

	body, err := toAPI(reservation)
	if err != nil {
		return nil, err
	}
	return spec.CreateReservation201JSONResponse(body), nil
}

// GetReservation returns one reservation. Contract: 200 | 404.
func (s *Server) GetReservation(ctx context.Context, request spec.GetReservationRequestObject) (spec.GetReservationResponseObject, error) {
	reservation, err := s.svc.Get(ctx, request.ReservationId.String())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return spec.GetReservation404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.ReservationId))}, nil
		}
		return nil, err
	}

	body, err := toAPI(reservation)
	if err != nil {
		return nil, err
	}
	return spec.GetReservation200JSONResponse(body), nil
}

// ConfirmReservation promotes a held reservation. Contract: 200 | 404 | 422. Idempotent.
func (s *Server) ConfirmReservation(ctx context.Context, request spec.ConfirmReservationRequestObject) (spec.ConfirmReservationResponseObject, error) {
	reservation, err := s.svc.Confirm(ctx, request.ReservationId.String())
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return spec.ConfirmReservation404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.ReservationId))}, nil

		case errors.Is(err, domain.ErrAlreadyReleased):
			// 422, not 409: released is terminal, so no amount of retrying makes this succeed.
			return spec.ConfirmReservation422JSONResponse(errorBody(ctx, spec.ReservationAlreadyReleased,
				"This reservation was already released and cannot be confirmed.")), nil
		}
		return nil, err
	}

	body, err := toAPI(reservation)
	if err != nil {
		return nil, err
	}
	return spec.ConfirmReservation200JSONResponse(body), nil
}

// ReleaseReservation frees a window. Contract: 200 | 404. Idempotent.
func (s *Server) ReleaseReservation(ctx context.Context, request spec.ReleaseReservationRequestObject) (spec.ReleaseReservationResponseObject, error) {
	reason := defaultReleaseReason
	if request.Body != nil {
		reason = domain.ReleaseReason(request.Body.Reason)
	}

	reservation, err := s.svc.Release(ctx, request.ReservationId.String(), reason)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return spec.ReleaseReservation404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.ReservationId))}, nil

		case errors.Is(err, domain.ErrInvalidReleaseReason):
			// The spec documents no 400 on this operation, so this cannot be rendered as the
			// contract's validation error. It is returned as a 500 rather than silently coerced
			// to a valid reason, which would put a fabricated cause in the audit trail and in
			// the ReservationReleased event Booking consumes.
			//
			// FLAGGED as a contract gap: releaseReservation needs a documented 400 response.
			s.log.WarnContext(ctx, "release reason outside the contract enum; the spec documents no 400 here",
				slog.String("reservation_id", request.ReservationId.String()),
				slog.String("reason", string(reason)))
			return nil, fmt.Errorf("api: %w", err)
		}
		return nil, err
	}

	body, err := toAPI(reservation)
	if err != nil {
		return nil, err
	}
	return spec.ReleaseReservation200JSONResponse(body), nil
}

// ListSlots reports occupied windows. Contract: 200 | 400 | 404.
//
// Advisory only, and the spec is emphatic about it: a window free here can be taken before the
// caller reserves it. Only the reserve is authoritative.
func (s *Server) ListSlots(ctx context.Context, request spec.ListSlotsRequestObject) (spec.ListSlotsResponseObject, error) {
	windows, err := s.svc.Busy(ctx, request.ResourceId.String(), request.Params.From, request.Params.To)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidQueryWindow) {
			return spec.ListSlots400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
				errorBody(ctx, spec.ValidationFailed, "`to` must be after `from`.",
					detail("to", "must be after `from`")))}, nil
		}
		return nil, err
	}

	busy := make([]struct {
		EndsAt   time.Time `json:"ends_at"`
		StartsAt time.Time `json:"starts_at"`
	}, 0, len(windows))
	for _, w := range windows {
		busy = append(busy, struct {
			EndsAt   time.Time `json:"ends_at"`
			StartsAt time.Time `json:"starts_at"`
		}{EndsAt: w.EndsAt, StartsAt: w.StartsAt})
	}

	return spec.ListSlots200JSONResponse{
		ResourceId: request.ResourceId,
		Busy:       busy,
	}, nil
}

// AvailabilityHealthz and AvailabilityReadyz are declared by the generated interface because the
// spec documents them, but they are NOT reachable: the probes are served by
// shared/platform/health, mounted on the outer mux ahead of this handler (see cmd/availability).
//
// They are implemented as loud failures rather than as a cheerful `{"status":"ok"}`. A readiness
// probe that reports ready without running a single dependency check is worse than no probe —
// it keeps a broken instance in the load balancer — so if the routing ever changes such that
// these are reached, that must be obvious immediately.
func (s *Server) AvailabilityHealthz(ctx context.Context, _ spec.AvailabilityHealthzRequestObject) (spec.AvailabilityHealthzResponseObject, error) {
	return nil, errUnreachableProbe
}

func (s *Server) AvailabilityReadyz(ctx context.Context, _ spec.AvailabilityReadyzRequestObject) (spec.AvailabilityReadyzResponseObject, error) {
	return nil, errUnreachableProbe
}

var errUnreachableProbe = errors.New(
	"api: health probes are served by shared/platform/health on the outer mux; reaching this " +
		"handler means the routing in cmd/availability changed and readiness is no longer checking anything")

// validateIdempotencyKey returns a human-readable problem, or "" when the key is usable.
func validateIdempotencyKey(key string) string {
	switch {
	case key == "":
		return "is required"
	case len(key) < idempotencyKeyMinLen:
		return fmt.Sprintf("must be at least %d characters", idempotencyKeyMinLen)
	case len(key) > idempotencyKeyMaxLen:
		return fmt.Sprintf("must be at most %d characters", idempotencyKeyMaxLen)
	default:
		return ""
	}
}

// fingerprintOf digests the DECODED body rather than the raw bytes.
//
// That is deliberate. A client retrying the identical request may re-serialise it with different
// key ordering or whitespace; hashing the raw bytes would call that a different body and answer
// a legitimate retry with 409 idempotency_key_reuse. Re-marshalling the typed struct normalises
// field order and formatting, so the digest reflects what was actually asked for.
func fingerprintOf(body spec.CreateReservationJSONRequestBody) (string, error) {
	canonical, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("api: fingerprinting request body: %w", err)
	}
	return idempotency.Fingerprint(canonical), nil
}

// toAPI maps a domain reservation onto the contract's Reservation schema.
func toAPI(r domain.Reservation) (spec.Reservation, error) {
	id, err := uuid.Parse(r.ID)
	if err != nil {
		return spec.Reservation{}, fmt.Errorf("api: reservation id %q is not a uuid: %w", r.ID, err)
	}
	resourceID, err := uuid.Parse(r.ResourceID)
	if err != nil {
		return spec.Reservation{}, fmt.Errorf("api: resource id %q is not a uuid: %w", r.ResourceID, err)
	}
	bookingID, err := uuid.Parse(r.BookingID)
	if err != nil {
		return spec.Reservation{}, fmt.Errorf("api: booking id %q is not a uuid: %w", r.BookingID, err)
	}

	out := spec.Reservation{
		Id:         id,
		ResourceId: resourceID,
		BookingId:  bookingID,
		StartsAt:   r.StartsAt.UTC(),
		EndsAt:     r.EndsAt.UTC(),
		Status:     spec.ReservationStatus(r.Status),
		CreatedAt:  r.CreatedAt.UTC(),
		UpdatedAt:  r.UpdatedAt.UTC(),
	}
	if r.ExpiresAt != nil {
		utc := r.ExpiresAt.UTC()
		out.ExpiresAt = &utc
	}
	if r.ReleasedReason != nil {
		reason := spec.ReleaseReason(*r.ReleasedReason)
		out.ReleasedReason = &reason
	}
	return out, nil
}

func notFoundBody(ctx context.Context, id uuid.UUID) spec.Error {
	return errorBody(ctx, spec.ReservationNotFound,
		fmt.Sprintf("No reservation with id %s.", id))
}

// detail is one field-level problem, matching the spec's details[] items.
type errDetail = struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

func detail(field, issue string) errDetail {
	return errDetail{Field: field, Issue: issue}
}

// errorBody builds the contract error envelope.
//
// The correlation ID is always present, so a customer quoting the id from an error page leads
// straight to the logs and trace for that exact request.
func errorBody(ctx context.Context, code spec.ErrorErrorCode, message string, details ...errDetail) spec.Error {
	var e spec.Error
	e.Error.Code = code
	e.Error.Message = message
	e.Error.CorrelationId = correlation.FromContext(ctx)
	if len(details) > 0 {
		e.Error.Details = &details
	}
	return e
}

// WriteError renders the contract envelope for failures raised outside a strict handler —
// parameter binding, body decoding, and unhandled handler errors.
//
// Without it those paths emit oapi-codegen's plain-text default, which is a 400 or 500 whose
// body does not match the spec's Error schema. AC5 covers every non-2xx, including these.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code spec.ErrorErrorCode, message string) {
	body := errorBody(r.Context(), code, message)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
