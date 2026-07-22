package infra

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	catalog "github.com/morisempai/wakewake/shared/contracts/openapi/catalog"
	"github.com/morisempai/wakewake/shared/platform/httpx"

	"github.com/morisempai/wakewake/services/booking/internal/domain"
)

// CatalogClient resolves a product through the Catalog HTTP API (getProduct) using the committed
// generated client. Catalog owns the product -> resource mapping, the capacity, and the price;
// booking reads them here and never touches catalog's tables (hard rule #6).
type CatalogClient struct {
	client catalog.ClientWithResponsesInterface
}

var _ domain.Catalog = (*CatalogClient)(nil)

// NewCatalogClient builds the client with a correlation-propagating, retrying http.Client. GET is
// idempotent, so it is safe to retry on a transient failure.
func NewCatalogClient(baseURL string) (*CatalogClient, error) {
	client, err := catalog.NewClientWithResponses(baseURL,
		catalog.WithHTTPClient(httpx.NewClient(httpx.ClientConfig{})))
	if err != nil {
		return nil, fmt.Errorf("booking: building catalog client: %w", err)
	}
	return &CatalogClient{client: client}, nil
}

// Product returns the product, or ErrProductNotFound for an unknown id.
func (c *CatalogClient) Product(ctx context.Context, productID string) (domain.Product, error) {
	id, err := uuid.Parse(productID)
	if err != nil {
		// A non-uuid product id can never exist; report it as not found rather than as a transport
		// error the caller would retry.
		return domain.Product{}, domain.ErrProductNotFound
	}

	res, err := c.client.GetProductWithResponse(ctx, id, &catalog.GetProductParams{})
	if err != nil {
		// Catalog unreachable. Reuse the availability-unavailable outcome so the caller gets the
		// same "dependency down, no booking created, safe to retry" 503. See README: booking.yaml
		// has no dedicated catalog-unavailable code, flagged as a contract-change candidate.
		return domain.Product{}, fmt.Errorf("%w: catalog: %v", domain.ErrAvailabilityUnavailable, err)
	}

	switch res.StatusCode() {
	case http.StatusOK:
		if res.JSON200 == nil {
			return domain.Product{}, fmt.Errorf("%w: catalog 200 with no body", domain.ErrAvailabilityUnavailable)
		}
		p := res.JSON200
		return domain.Product{
			ResourceID: p.ResourceId.String(),
			Capacity:   p.Capacity,
			PriceMinor: int64(p.BasePriceMinor),
			Currency:   p.Currency,
		}, nil

	case http.StatusNotFound:
		return domain.Product{}, domain.ErrProductNotFound

	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusInternalServerError:
		return domain.Product{}, fmt.Errorf("%w: catalog returned %d", domain.ErrAvailabilityUnavailable, res.StatusCode())

	default:
		return domain.Product{}, fmt.Errorf("booking: unexpected catalog status %d for product %s", res.StatusCode(), productID)
	}
}
