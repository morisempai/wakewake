package infra

import (
	"context"
	"fmt"

	"github.com/morisempai/wakewake/services/notification/internal/domain"
)

// DevRecipientResolver is a DEVELOPMENT-ONLY stub for the recipient lookup.
//
// KNOWN GAP (issue #19): BookingConfirmed deliberately carries no email or name (contract
// x-notes, NFR-4), and there is no customer/identity service in this slice to ask. Rather than
// invent one or query another service's database (hard rule #6), this resolver derives a
// deterministic, obviously-non-deliverable address from the customer id:
//
//	customer-<customer_id>@example.test
//
// `.test` is a reserved TLD (RFC 6761), so nothing here can ever reach a real inbox. A production
// resolver must call the real identity source over its API; wiring that is out of scope for this
// slice and flagged in the PR as the follow-up that closes this gap.
type DevRecipientResolver struct{}

// NewDevRecipientResolver constructs the stub resolver.
func NewDevRecipientResolver() DevRecipientResolver { return DevRecipientResolver{} }

// Resolve returns the deterministic dev address for a customer id.
func (DevRecipientResolver) Resolve(_ context.Context, customerID string) (domain.Recipient, error) {
	if customerID == "" {
		return "", fmt.Errorf("infra: cannot resolve a recipient for an empty customer id")
	}
	return domain.Recipient(fmt.Sprintf("customer-%s@example.test", customerID)), nil
}
