// Package api is the HTTP layer: it binds requests, calls one domain method, and maps the result
// onto the responses contracts/openapi/booking.yaml declares.
//
// It contains no business decisions. The types and the routing come from oapi-codegen against the
// spec (shared/contracts/openapi/booking), so a response shape that drifts from the contract is a
// compile error rather than something a contract test has to notice later.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/booking"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/idempotency"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// Idempotency-Key bounds from the spec's `minLength: 8, maxLength: 255`. Enforced here because
// oapi-codegen binds header parameters without applying string constraints.
const (
	idempotencyKeyMinLen = 8
	idempotencyKeyMaxLen = 255
)

// defaultListLimit mirrors the spec's `limit` default.
const defaultListLimit = 50

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

// CreateBooking holds a slot — the first saga step. Contract: 201 | 400 | 401 | 404 | 409 | 422 | 503.
func (s *Server) CreateBooking(ctx context.Context, request spec.CreateBookingRequestObject) (spec.CreateBookingResponseObject, error) {
	customer, ok := customerID(ctx)
	if !ok {
		return spec.CreateBooking401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	if request.Body == nil {
		return spec.CreateBooking400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "A JSON request body is required."))}, nil
	}

	if problem := validateIdempotencyKey(request.Params.IdempotencyKey); problem != "" {
		return spec.CreateBooking400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "The Idempotency-Key header is not usable.",
				detail("Idempotency-Key", problem)))}, nil
	}

	if request.Body.PartySize < 1 {
		return spec.CreateBooking400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "party_size must be at least 1.",
				detail("party_size", "must be at least 1")))}, nil
	}

	fingerprint, err := fingerprintOf(*request.Body)
	if err != nil {
		return nil, err
	}

	booking, err := s.svc.CreateBooking(ctx, domain.CreateCommand{
		CustomerID: customer,
		ProductID:  request.Body.ProductId.String(),
		StartsAt:   request.Body.StartsAt,
		EndsAt:     request.Body.EndsAt,
		PartySize:  request.Body.PartySize,
		Idempotency: domain.Claim{
			Key:         request.Params.IdempotencyKey,
			Fingerprint: fingerprint,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrMissingIdentifier):
			return spec.CreateBooking400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
				errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "product_id is required."))}, nil

		case errors.Is(err, domain.ErrProductNotFound):
			return spec.CreateBooking404JSONResponse(errorBody(ctx, spec.ErrorErrorCodeProductNotFound,
				"No product with that id.")), nil

		case errors.Is(err, domain.ErrInvalidTimeWindow):
			return spec.CreateBooking422JSONResponse(errorBody(ctx, spec.ErrorErrorCodeInvalidTimeWindow,
				"ends_at must be strictly after starts_at, and the window must not be in the past.",
				detail("ends_at", "must be strictly after starts_at"))), nil

		case errors.Is(err, domain.ErrPartyExceedsCapacity):
			return spec.CreateBooking422JSONResponse(errorBody(ctx, spec.ErrorErrorCodePartyExceedsCapacity,
				"party_size exceeds the product's capacity.",
				detail("party_size", "exceeds the product's capacity"))), nil

		case errors.Is(err, domain.ErrSlotUnavailable):
			s.log.InfoContext(ctx, "hold lost the race for the window",
				slog.String("product_id", request.Body.ProductId.String()))
			return spec.CreateBooking409JSONResponse(errorBody(ctx, spec.ErrorErrorCodeSlotUnavailable,
				"The slot is no longer available.")), nil

		case errors.Is(err, domain.ErrIdempotencyKeyReuse):
			return spec.CreateBooking409JSONResponse(errorBody(ctx, spec.ErrorErrorCodeIdempotencyKeyReuse,
				"This Idempotency-Key was already used with a different request body.",
				detail("Idempotency-Key", "already used with a different request body"))), nil

		case errors.Is(err, domain.ErrAvailabilityUnavailable):
			s.log.WarnContext(ctx, "a downstream dependency was unavailable; no hold created",
				slog.String("error", err.Error()))
			return spec.CreateBooking503JSONResponse(errorBody(ctx, spec.ErrorErrorCodeAvailabilityUnavailable,
				"A dependency is unavailable and no hold was created. Safe to retry.")), nil
		}
		return nil, err
	}

	body, err := toAPI(booking)
	if err != nil {
		return nil, err
	}
	return spec.CreateBooking201JSONResponse(body), nil
}

// ListBookings lists the caller's own bookings, newest first. Contract: 200 | 401.
func (s *Server) ListBookings(ctx context.Context, request spec.ListBookingsRequestObject) (spec.ListBookingsResponseObject, error) {
	customer, ok := customerID(ctx)
	if !ok {
		return spec.ListBookings401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	q := domain.ListQuery{CustomerID: customer, Limit: defaultListLimit}
	if request.Params.Limit != nil {
		q.Limit = *request.Params.Limit
	}
	if request.Params.Cursor != nil {
		q.Cursor = *request.Params.Cursor
	}
	if request.Params.Status != nil {
		st := domain.Status(*request.Params.Status)
		q.Status = &st
	}

	page, err := s.svc.List(ctx, q)
	if err != nil {
		return nil, err
	}

	data := make([]spec.Booking, 0, len(page.Bookings))
	for _, b := range page.Bookings {
		api, err := toAPI(b)
		if err != nil {
			return nil, err
		}
		data = append(data, api)
	}

	return spec.ListBookings200JSONResponse{Data: data, NextCursor: page.NextCursor}, nil
}

// GetBooking returns one booking. Contract: 200 | 401 | 403 | 404.
func (s *Server) GetBooking(ctx context.Context, request spec.GetBookingRequestObject) (spec.GetBookingResponseObject, error) {
	customer, ok := customerID(ctx)
	if !ok {
		return spec.GetBooking401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	booking, err := s.svc.Get(ctx, request.BookingId.String(), customer)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return spec.GetBooking404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.BookingId))}, nil
		case errors.Is(err, domain.ErrForbidden):
			return spec.GetBooking403JSONResponse(forbiddenBody(ctx)), nil
		}
		return nil, err
	}

	body, err := toAPI(booking)
	if err != nil {
		return nil, err
	}
	return spec.GetBooking200JSONResponse(body), nil
}

// CancelBooking cancels a booking and releases its slot. Contract: 200 | 401 | 403 | 404. Idempotent.
func (s *Server) CancelBooking(ctx context.Context, request spec.CancelBookingRequestObject) (spec.CancelBookingResponseObject, error) {
	customer, ok := customerID(ctx)
	if !ok {
		return spec.CancelBooking401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	var note *string
	if request.Body != nil {
		note = request.Body.Reason
	}

	booking, err := s.svc.Cancel(ctx, request.BookingId.String(), customer, note)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrNotFound):
			return spec.CancelBooking404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.BookingId))}, nil
		case errors.Is(err, domain.ErrForbidden):
			return spec.CancelBooking403JSONResponse(forbiddenBody(ctx)), nil
		}
		return nil, err
	}

	body, err := toAPI(booking)
	if err != nil {
		return nil, err
	}
	return spec.CancelBooking200JSONResponse(body), nil
}

// BookingHealthz and BookingReadyz are declared by the generated interface because the spec
// documents them, but they are NOT reachable: the probes are served by shared/platform/health,
// mounted on the outer mux ahead of this handler (see cmd/booking). They fail loudly so that a
// routing change that ever reaches them is obvious immediately.
func (s *Server) BookingHealthz(context.Context, spec.BookingHealthzRequestObject) (spec.BookingHealthzResponseObject, error) {
	return nil, errUnreachableProbe
}

func (s *Server) BookingReadyz(context.Context, spec.BookingReadyzRequestObject) (spec.BookingReadyzResponseObject, error) {
	return nil, errUnreachableProbe
}

var errUnreachableProbe = errors.New(
	"api: health probes are served by shared/platform/health on the outer mux; reaching this " +
		"handler means the routing in cmd/booking changed and readiness is no longer checking anything")

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

// fingerprintOf digests the DECODED body rather than the raw bytes, so a client retrying the
// identical request with different key ordering or whitespace is still recognised as a retry.
func fingerprintOf(body spec.CreateBookingJSONRequestBody) (string, error) {
	canonical, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("api: fingerprinting request body: %w", err)
	}
	return idempotency.Fingerprint(canonical), nil
}

// toAPI maps a domain booking onto the contract's Booking schema.
func toAPI(b domain.Booking) (spec.Booking, error) {
	id, err := uuid.Parse(b.ID)
	if err != nil {
		return spec.Booking{}, fmt.Errorf("api: booking id %q is not a uuid: %w", b.ID, err)
	}
	customerID, err := uuid.Parse(b.CustomerID)
	if err != nil {
		return spec.Booking{}, fmt.Errorf("api: customer id %q is not a uuid: %w", b.CustomerID, err)
	}
	productID, err := uuid.Parse(b.ProductID)
	if err != nil {
		return spec.Booking{}, fmt.Errorf("api: product id %q is not a uuid: %w", b.ProductID, err)
	}
	resourceID, err := uuid.Parse(b.ResourceID)
	if err != nil {
		return spec.Booking{}, fmt.Errorf("api: resource id %q is not a uuid: %w", b.ResourceID, err)
	}

	out := spec.Booking{
		Id:                 id,
		CustomerId:         customerID,
		ProductId:          productID,
		ResourceId:         resourceID,
		StartsAt:           b.StartsAt.UTC(),
		EndsAt:             b.EndsAt.UTC(),
		PartySize:          b.PartySize,
		Status:             spec.BookingStatus(b.Status),
		TotalMinor:         int(b.TotalMinor),
		Currency:           b.Currency,
		CancellationReason: b.CancellationReason,
		CreatedAt:          b.CreatedAt.UTC(),
		UpdatedAt:          b.UpdatedAt.UTC(),
	}
	if b.ReservationID != nil {
		rid, err := uuid.Parse(*b.ReservationID)
		if err != nil {
			return spec.Booking{}, fmt.Errorf("api: reservation id %q is not a uuid: %w", *b.ReservationID, err)
		}
		out.ReservationId = &rid
	}
	if b.HoldExpiresAt != nil {
		utc := b.HoldExpiresAt.UTC()
		out.HoldExpiresAt = &utc
	}
	return out, nil
}

func notFoundBody(ctx context.Context, id uuid.UUID) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodeBookingNotFound, fmt.Sprintf("No booking with id %s.", id))
}

func forbiddenBody(ctx context.Context) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodeUnauthorized, "This booking belongs to another customer.")
}

func unauthenticatedBody(ctx context.Context) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodeUnauthenticated, "A valid bearer token is required.")
}

// detail is one field-level problem, matching the spec's details[] items.
type errDetail = struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

func detail(field, issue string) errDetail {
	return errDetail{Field: field, Issue: issue}
}

// errorBody builds the contract error envelope. The correlation id is always present, so a customer
// quoting the id from an error page leads straight to the logs and trace for that exact request.
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

// WriteError renders the contract envelope for failures raised outside a strict handler — parameter
// binding, body decoding, and unhandled handler errors. Without it those paths emit oapi-codegen's
// plain-text default, whose body does not match the spec's Error schema.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code spec.ErrorErrorCode, message string) {
	body := errorBody(r.Context(), code, message)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
