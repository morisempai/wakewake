package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/payment"
	"github.com/morisempai/wakewake/shared/platform/correlation"

	"github.com/morisempai/wakewake/services/payment/internal/domain"
	"github.com/morisempai/wakewake/services/payment/internal/stripe"
)

// maxWebhookBody caps the body we read. A Stripe event is small; an unbounded read is a trivial
// memory-exhaustion vector on an unauthenticated endpoint.
const maxWebhookBody = 1 << 20 // 1 MiB

// outcomeRecorder is the slice of the store the webhook needs: apply a verified outcome atomically.
type outcomeRecorder interface {
	RecordOutcome(ctx context.Context, stripeEventID, stripeType, providerPaymentID string, fn domain.Transition) (domain.OutcomeResult, error)
}

// WebhookHandler serves POST /v1/webhooks/stripe. It is a RAW handler, not a strict-server method,
// for one PCI-critical reason: the signature must be verified over the exact bytes Stripe sent, and
// the generated strict server decodes the JSON body before any handler runs, destroying those
// bytes. So this reads the body itself, verifies the signature FIRST, and only then parses.
//
// An invalid, absent, or expired signature is rejected with the contract's 400 and mutates nothing
// and emits nothing. Secrets and the raw body are never logged.
type WebhookHandler struct {
	svc      *domain.Service
	recorder outcomeRecorder
	log      *slog.Logger

	webhookSecret string
	tolerance     time.Duration
	now           func() time.Time
}

// NewWebhookHandler wires the webhook. now is injected so signature-tolerance checks are
// deterministic in tests.
func NewWebhookHandler(svc *domain.Service, recorder outcomeRecorder, log *slog.Logger, webhookSecret string, tolerance time.Duration, now func() time.Time) *WebhookHandler {
	if now == nil {
		now = time.Now
	}
	return &WebhookHandler{
		svc:           svc,
		recorder:      recorder,
		log:           log.With(slog.String("handler", "stripe_webhook")),
		webhookSecret: webhookSecret,
		tolerance:     tolerance,
		now:           now,
	}
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Read the raw bytes. Everything below verifies and parses THESE bytes — never a re-serialised
	// copy — so the HMAC matches what Stripe signed.
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		h.reject(ctx, w, spec.ErrorErrorCodeValidationFailed, "The request body could not be read.")
		return
	}

	// Verify BEFORE trusting anything. An unverified body is attacker-controlled input that could
	// mark any booking paid (payment.yaml). The error is logged without the secret, the signature,
	// or the body — deliberately terse, because a verbose failure is a forging oracle.
	sigHeader := r.Header.Get("Stripe-Signature")
	if err := stripe.VerifySignature(body, sigHeader, h.webhookSecret, h.now(), h.tolerance); err != nil {
		h.log.WarnContext(ctx, "rejecting webhook with an invalid signature")
		h.reject(ctx, w, spec.ErrorErrorCodeSignatureVerificationFailed, "The webhook signature could not be verified.")
		return
	}

	event, err := stripe.ParseEvent(body)
	if err != nil {
		h.log.WarnContext(ctx, "rejecting a verified-but-malformed webhook body")
		h.reject(ctx, w, spec.ErrorErrorCodeValidationFailed, "The webhook body is malformed.")
		return
	}

	// Unknown event types are acknowledged and ignored (payment.yaml). Nothing to emit.
	if !event.IsPaymentOutcome() {
		h.log.DebugContext(ctx, "acknowledging an ignored webhook event type",
			slog.String("stripe_event", event.ID), slog.String("type", event.Type))
		h.acknowledge(w)
		return
	}

	outcome := domain.WebhookOutcome{
		StripeEventID:     event.ID,
		StripeType:        event.Type,
		ProviderPaymentID: event.PaymentIntentID,
		Succeeded:         event.Succeeded(),
		FailureCode:       event.FailureCode,
	}

	res, err := h.recorder.RecordOutcome(ctx, event.ID, event.Type, event.PaymentIntentID, h.svc.OutcomeTransition(outcome))
	if err != nil {
		// A genuine infra failure. Return non-2xx so Stripe retries — a 200 here would drop the
		// outcome permanently. Not one of the contract's documented webhook responses, but the
		// operationally correct answer to "our database is momentarily down".
		h.log.ErrorContext(ctx, "failed to record a verified webhook outcome",
			slog.String("stripe_event", event.ID), slog.String("error", err.Error()))
		WriteError(w, r, http.StatusInternalServerError, spec.ErrorErrorCodeInternalError,
			"The webhook could not be processed. Safe to retry.")
		return
	}

	// Re-hydrate the original correlation id captured at createPayment time (issue #23) so the
	// processing log lines below join the same trace as the original request, rather than the id
	// correlation.Middleware minted for this header-less external callback. The events themselves are
	// already stamped inside RecordOutcome; this covers the handler's own logs. A legacy payment (no
	// stored id) leaves the minted id in place.
	if res.Found && res.Payment.CorrelationID != "" {
		ctx = correlation.WithID(ctx, res.Payment.CorrelationID)
	}

	switch {
	case !res.Found:
		// No local payment for this intent. Acknowledge so Stripe stops retrying; there is nothing
		// we can do with an outcome for a payment we never created.
		h.log.WarnContext(ctx, "verified webhook for an unknown payment intent",
			slog.String("stripe_event", event.ID))
	case res.Duplicate:
		h.log.DebugContext(ctx, "duplicate webhook ignored", slog.String("stripe_event", event.ID))
	case res.Changed:
		h.log.InfoContext(ctx, "recorded payment outcome",
			slog.String("stripe_event", event.ID),
			slog.String("payment_id", res.Payment.ID),
			slog.String("status", string(res.Payment.Status)))
	default:
		h.log.InfoContext(ctx, "webhook applied no change; payment was already terminal",
			slog.String("stripe_event", event.ID), slog.String("payment_id", res.Payment.ID))
	}

	h.acknowledge(w)
}

// acknowledge writes the contract's 200 {"received": true}.
func (h *WebhookHandler) acknowledge(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]bool{"received": true})
}

// reject writes the contract's 400 with the given code. Deliberately gives no detail beyond the
// code and a generic message.
func (h *WebhookHandler) reject(ctx context.Context, w http.ResponseWriter, code spec.ErrorErrorCode, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(errorBody(ctx, code, message))
}
