// Package api is the HTTP layer: it binds requests, calls one domain method, and maps the result
// onto the responses contracts/openapi/payment.yaml declares.
//
// It contains no business decisions. The types and the routing come from oapi-codegen against the
// spec (shared/contracts/openapi/payment), so a response shape that drifts from the contract is a
// compile error rather than something a contract test has to notice later.
//
// The Stripe webhook is the exception to "everything through the strict server": it needs the RAW
// request body for signature verification, which the generated strict handler destroys by decoding
// the body itself. It is served by a dedicated raw handler mounted ahead of the generated routes
// (see webhook.go and router.go); the strict method here is deliberately unreachable.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/payment"
	"github.com/morisempai/wakewake/shared/platform/correlation"
	"github.com/morisempai/wakewake/shared/platform/idempotency"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
)

// Idempotency-Key bounds from the spec's `minLength: 8, maxLength: 255`. Enforced here because
// oapi-codegen binds header parameters without applying string constraints.
const (
	idempotencyKeyMinLen = 8
	idempotencyKeyMaxLen = 255
)

// Server implements the generated strict server interface for the JSON endpoints.
type Server struct {
	svc *domain.Service
	log *slog.Logger
}

// NewServer wires the HTTP layer.
func NewServer(svc *domain.Service, log *slog.Logger) *Server {
	return &Server{svc: svc, log: log}
}

var _ spec.StrictServerInterface = (*Server)(nil)

// CreatePayment starts a payment for a held booking. Contract: 201 | 400 | 401 | 404 | 409 | 422 | 502.
func (s *Server) CreatePayment(ctx context.Context, request spec.CreatePaymentRequestObject) (spec.CreatePaymentResponseObject, error) {
	if _, ok := customerID(ctx); !ok {
		return spec.CreatePayment401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	if request.Body == nil {
		return spec.CreatePayment400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "A JSON request body is required."))}, nil
	}

	if problem := validateIdempotencyKey(request.Params.IdempotencyKey); problem != "" {
		return spec.CreatePayment400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "The Idempotency-Key header is not usable.",
				detail("Idempotency-Key", problem)))}, nil
	}

	bookingID := request.Body.BookingId
	if bookingID == uuid.Nil {
		return spec.CreatePayment400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
			errorBody(ctx, spec.ErrorErrorCodeValidationFailed, "booking_id is required.",
				detail("booking_id", "is required")))}, nil
	}

	fingerprint, err := fingerprintOf(*request.Body)
	if err != nil {
		return nil, err
	}

	payment, clientSecret, err := s.svc.CreatePayment(ctx, domain.CreateCommand{
		BookingID: bookingID.String(),
		Idempotency: domain.Claim{
			Key:         request.Params.IdempotencyKey,
			Fingerprint: fingerprint,
		},
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrBookingNotFound):
			return spec.CreatePayment404JSONResponse(errorBody(ctx, spec.ErrorErrorCodeBookingNotFound,
				"No booking with that id.")), nil

		case errors.Is(err, domain.ErrBookingNotPayable):
			return spec.CreatePayment422JSONResponse(errorBody(ctx, spec.ErrorErrorCodeBookingNotPayable,
				"The booking is not payable: its hold has expired.",
				detail("booking_id", "the hold has expired"))), nil

		case errors.Is(err, domain.ErrPaymentAlreadyExists):
			return spec.CreatePayment409JSONResponse(errorBody(ctx, spec.ErrorErrorCodePaymentAlreadyExists,
				"This booking already has a payment in flight.")), nil

		case errors.Is(err, domain.ErrIdempotencyKeyReuse):
			return spec.CreatePayment409JSONResponse(errorBody(ctx, spec.ErrorErrorCodeIdempotencyKeyReuse,
				"This Idempotency-Key was already used with a different request body.",
				detail("Idempotency-Key", "already used with a different request body"))), nil

		case errors.Is(err, domain.ErrProviderError):
			// The underlying Stripe error is logged, never echoed: a 502 body that leaks a provider
			// message hands out internals. No charge was made, so the client may retry.
			s.log.WarnContext(ctx, "stripe rejected or was unreachable; no charge made",
				slog.String("error", err.Error()))
			return spec.CreatePayment502JSONResponse(errorBody(ctx, spec.ErrorErrorCodeProviderError,
				"The payment provider was unavailable and no charge was made. Safe to retry.")), nil
		}
		return nil, err
	}

	return toCreateResponse(payment, clientSecret)
}

// GetPayment returns one payment's status. Contract: 200 | 401 | 403 | 404.
func (s *Server) GetPayment(ctx context.Context, request spec.GetPaymentRequestObject) (spec.GetPaymentResponseObject, error) {
	customer, ok := customerID(ctx)
	if !ok {
		return spec.GetPayment401JSONResponse{UnauthorizedJSONResponse: spec.UnauthorizedJSONResponse(
			unauthenticatedBody(ctx))}, nil
	}

	payment, err := s.svc.Get(ctx, request.PaymentId.String(), customer)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrPaymentNotFound):
			return spec.GetPayment404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.PaymentId))}, nil
		case errors.Is(err, domain.ErrForbidden):
			return spec.GetPayment403JSONResponse(forbiddenBody(ctx)), nil
		}
		return nil, err
	}

	body, err := toPayment(payment)
	if err != nil {
		return nil, err
	}
	return spec.GetPayment200JSONResponse(body), nil
}

// PaymentHealthz, PaymentReadyz, and HandleStripeWebhook are declared by the generated interface
// because the spec documents them, but none is reachable through the strict server: the probes are
// served by shared/platform/health and the webhook by the raw handler in webhook.go, both mounted
// ahead of the generated routes (see router.go). They fail loudly so that a routing change which
// ever reaches them is obvious immediately.
func (s *Server) PaymentHealthz(context.Context, spec.PaymentHealthzRequestObject) (spec.PaymentHealthzResponseObject, error) {
	return nil, errUnreachableProbe
}

func (s *Server) PaymentReadyz(context.Context, spec.PaymentReadyzRequestObject) (spec.PaymentReadyzResponseObject, error) {
	return nil, errUnreachableProbe
}

func (s *Server) HandleStripeWebhook(context.Context, spec.HandleStripeWebhookRequestObject) (spec.HandleStripeWebhookResponseObject, error) {
	return nil, errUnreachableWebhook
}

var (
	errUnreachableProbe = errors.New(
		"api: health probes are served by shared/platform/health on the outer mux; reaching this " +
			"handler means the routing in cmd/payment changed and readiness is no longer checking anything")

	errUnreachableWebhook = errors.New(
		"api: the Stripe webhook is served by the raw handler in webhook.go, which needs the raw body " +
			"for signature verification; reaching the generated strict handler means the raw route was " +
			"unmounted and signatures are being verified against a re-serialised body — a PCI regression")
)

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
func fingerprintOf(body spec.CreatePaymentJSONRequestBody) (string, error) {
	canonical, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("api: fingerprinting request body: %w", err)
	}
	return idempotency.Fingerprint(canonical), nil
}

// toCreateResponse maps a domain payment plus the one-time client secret onto the 201 body.
func toCreateResponse(p domain.Payment, clientSecret string) (spec.CreatePaymentResponseObject, error) {
	id, bookingID, err := paymentUUIDs(p)
	if err != nil {
		return nil, err
	}
	return spec.CreatePayment201JSONResponse{
		Id:                id,
		BookingId:         bookingID,
		Status:            spec.PaymentStatus(p.Status),
		AmountMinor:       int(p.AmountMinor),
		Currency:          p.Currency,
		Provider:          spec.CreatePayment201JSONResponseBodyProviderStripe,
		ProviderPaymentId: p.ProviderPaymentID,
		ClientSecret:      clientSecret,
		FailureCode:       p.FailureCode,
		CreatedAt:         p.CreatedAt.UTC(),
		UpdatedAt:         p.UpdatedAt.UTC(),
	}, nil
}

// toPayment maps a domain payment onto the contract's Payment schema. It never carries a client
// secret — that is returned only on creation (payment.yaml).
func toPayment(p domain.Payment) (spec.Payment, error) {
	id, bookingID, err := paymentUUIDs(p)
	if err != nil {
		return spec.Payment{}, err
	}
	return spec.Payment{
		Id:                id,
		BookingId:         bookingID,
		Status:            spec.PaymentStatus(p.Status),
		AmountMinor:       int(p.AmountMinor),
		Currency:          p.Currency,
		Provider:          spec.PaymentProviderStripe,
		ProviderPaymentId: p.ProviderPaymentID,
		FailureCode:       p.FailureCode,
		CreatedAt:         p.CreatedAt.UTC(),
		UpdatedAt:         p.UpdatedAt.UTC(),
	}, nil
}

func paymentUUIDs(p domain.Payment) (uuid.UUID, uuid.UUID, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("api: payment id %q is not a uuid: %w", p.ID, err)
	}
	bookingID, err := uuid.Parse(p.BookingID)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("api: booking id %q is not a uuid: %w", p.BookingID, err)
	}
	return id, bookingID, nil
}

func notFoundBody(ctx context.Context, id uuid.UUID) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodePaymentNotFound, fmt.Sprintf("No payment with id %s.", id))
}

func forbiddenBody(ctx context.Context) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodeUnauthorized, "This payment belongs to another customer.")
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
