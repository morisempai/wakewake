// Package api is the HTTP layer: it binds requests, calls one domain method, and maps the result
// onto the responses contracts/openapi/catalog.yaml declares.
//
// It contains no business decisions. The types and the routing come from oapi-codegen against the
// spec (shared/contracts/openapi/catalog), so a response shape that drifts from the contract is a
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

	spec "github.com/morisempai/wakewake/shared/contracts/openapi/catalog"
	"github.com/morisempai/wakewake/shared/platform/correlation"

	"github.com/morisempai/wakewake/services/catalog/internal/domain"
)

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

// ListProducts returns a page of products, newest first. Contract: 200 | 400 | 401.
//
// oapi-codegen binds the query parameters but does NOT apply their schema constraints (the enum on
// `type`, the 1..200 bound on `limit`, the minimum 1 on `min_capacity`, the maxLength on
// `location`). Each is therefore re-checked here and rejected as 400 validation_failed, because a
// value the contract forbids must not reach the store as a query that quietly matches nothing.
func (s *Server) ListProducts(ctx context.Context, request spec.ListProductsRequestObject) (spec.ListProductsResponseObject, error) {
	p := request.Params

	limit := domain.DefaultLimit
	if p.Limit != nil {
		if *p.Limit < domain.MinLimit || *p.Limit > domain.MaxLimit {
			return s.listBadRequest(ctx, "limit",
				fmt.Sprintf("must be between %d and %d", domain.MinLimit, domain.MaxLimit)), nil
		}
		limit = *p.Limit
	}

	var filter domain.Filter

	if p.Type != nil {
		t := domain.ProductType(*p.Type)
		if !t.Valid() {
			return s.listBadRequest(ctx, "type", "must be one of boat, yacht, wakesurf_session"), nil
		}
		filter.Type = &t
	}

	if p.MinCapacity != nil {
		if *p.MinCapacity < 1 {
			return s.listBadRequest(ctx, "min_capacity", "must be at least 1"), nil
		}
		filter.MinCapacity = p.MinCapacity
	}

	if p.Location != nil {
		if len(*p.Location) > 100 {
			return s.listBadRequest(ctx, "location", "must be at most 100 characters"), nil
		}
		filter.Location = p.Location
	}

	var cursor *domain.Cursor
	if p.Cursor != nil {
		c, err := domain.DecodeCursor(*p.Cursor)
		if err != nil {
			return s.listBadRequest(ctx, "cursor", "is not a valid cursor"), nil
		}
		cursor = &c
	}

	page, err := s.svc.List(ctx, domain.ListQuery{Filter: filter, Cursor: cursor, Limit: limit})
	if err != nil {
		return nil, err
	}

	data := make([]spec.Product, 0, len(page.Products))
	for _, product := range page.Products {
		mapped, err := toAPI(product)
		if err != nil {
			return nil, err
		}
		data = append(data, mapped)
	}

	return spec.ListProducts200JSONResponse{Data: data, NextCursor: page.NextCursor}, nil
}

// GetProduct returns one product. Contract: 200 | 401 | 404.
func (s *Server) GetProduct(ctx context.Context, request spec.GetProductRequestObject) (spec.GetProductResponseObject, error) {
	product, err := s.svc.Get(ctx, request.ProductId.String())
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return spec.GetProduct404JSONResponse{NotFoundJSONResponse: spec.NotFoundJSONResponse(
				notFoundBody(ctx, request.ProductId))}, nil
		}
		return nil, err
	}

	body, err := toAPI(product)
	if err != nil {
		return nil, err
	}
	return spec.GetProduct200JSONResponse(body), nil
}

// listBadRequest builds the 400 for a rejected listing parameter.
func (s *Server) listBadRequest(ctx context.Context, field, issue string) spec.ListProducts400JSONResponse {
	s.log.InfoContext(ctx, "rejecting a listing parameter",
		slog.String("field", field), slog.String("issue", issue))
	return spec.ListProducts400JSONResponse{BadRequestJSONResponse: spec.BadRequestJSONResponse(
		errorBody(ctx, spec.ErrorErrorCodeValidationFailed,
			"One or more query parameters are invalid.", detail(field, issue)))}
}

// CatalogHealthz and CatalogReadyz are declared by the generated interface because the spec
// documents them, but they are NOT reachable: the probes are served by shared/platform/health,
// mounted on the outer mux ahead of this handler (see cmd/catalog).
//
// They are implemented as loud failures rather than a cheerful `{"status":"ok"}`. A readiness
// probe that reports ready without running a single dependency check is worse than no probe — it
// keeps a broken instance in the load balancer — so if the routing ever changes such that these
// are reached, that must be obvious immediately.
func (s *Server) CatalogHealthz(context.Context, spec.CatalogHealthzRequestObject) (spec.CatalogHealthzResponseObject, error) {
	return nil, errUnreachableProbe
}

func (s *Server) CatalogReadyz(context.Context, spec.CatalogReadyzRequestObject) (spec.CatalogReadyzResponseObject, error) {
	return nil, errUnreachableProbe
}

var errUnreachableProbe = errors.New(
	"api: health probes are served by shared/platform/health on the outer mux; reaching this " +
		"handler means the routing in cmd/catalog changed and readiness is no longer checking anything")

// toAPI maps a domain product onto the contract's Product schema.
func toAPI(p domain.Product) (spec.Product, error) {
	id, err := uuid.Parse(p.ID)
	if err != nil {
		return spec.Product{}, fmt.Errorf("api: product id %q is not a uuid: %w", p.ID, err)
	}
	resourceID, err := uuid.Parse(p.ResourceID)
	if err != nil {
		return spec.Product{}, fmt.Errorf("api: resource id %q is not a uuid: %w", p.ResourceID, err)
	}

	out := spec.Product{
		Id:             id,
		ResourceId:     resourceID,
		Type:           spec.ProductType(p.Type),
		Name:           p.Name,
		Description:    p.Description,
		Capacity:       p.Capacity,
		Location:       p.Location,
		BasePriceMinor: int(p.BasePriceMinor),
		Currency:       p.Currency,
		CreatedAt:      p.CreatedAt.UTC(),
		UpdatedAt:      p.UpdatedAt.UTC(),
	}
	// media_urls is optional (omitempty). Only attach it when there is something to show, so an
	// empty list is omitted rather than serialised as `[]`.
	if len(p.MediaURLs) > 0 {
		urls := append([]string(nil), p.MediaURLs...)
		out.MediaUrls = &urls
	}
	return out, nil
}

func notFoundBody(ctx context.Context, id uuid.UUID) spec.Error {
	return errorBody(ctx, spec.ErrorErrorCodeProductNotFound,
		fmt.Sprintf("No product with id %s.", id))
}

// errDetail is one field-level problem, matching the spec's details[] items.
type errDetail = struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

func detail(field, issue string) errDetail {
	return errDetail{Field: field, Issue: issue}
}

// errorBody builds the contract error envelope. The correlation ID is always present, so a
// customer quoting the id from an error page leads straight to the logs and trace for that request.
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
// parameter binding, and unhandled handler errors.
//
// Without it those paths emit oapi-codegen's plain-text default, which is a 400 or 500 whose body
// does not match the spec's Error schema. AC1 covers every response, including these.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code spec.ErrorErrorCode, message string) {
	body := errorBody(r.Context(), code, message)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
