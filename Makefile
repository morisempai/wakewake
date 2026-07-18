# Replaces the npm scripts that lived in package.json before ADR-0008.
#
# Two toolchains, on purpose: Go builds the services, Node runs the contract linters. Spectral
# and the AsyncAPI CLI lint YAML — the service language is irrelevant to them, and they are the
# best tools for the job regardless of what the services are written in. `.nvmrc` exists for
# these targets alone.

.PHONY: help infra-up infra-down infra-logs build test vet fmt lint contracts-lint contracts-lint-openapi contracts-lint-asyncapi check

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-24s %s\n", $$1, $$2}'

## --- infra ---------------------------------------------------------------

infra-up: ## Start Postgres, RabbitMQ, Keycloak, Vault, Mailhog, observability
	docker compose -f infra/docker-compose.yml up -d

infra-down: ## Stop and remove the local stack
	docker compose -f infra/docker-compose.yml down

infra-logs: ## Tail the local stack
	docker compose -f infra/docker-compose.yml logs -f

## --- go ------------------------------------------------------------------

build: ## Build every service module in the workspace
	go build ./...

test: ## Run unit tests with the race detector (testing-standards)
	go test -race ./...

vet: ## Static analysis
	go vet ./...

fmt: ## Format
	gofmt -w .

## --- contracts (Node tooling — see the header) ---------------------------

contracts-lint: contracts-lint-openapi contracts-lint-asyncapi ## Lint OpenAPI + AsyncAPI

contracts-lint-openapi: ## Spectral, using .spectral.yaml
	npx --yes @stoplight/spectral-cli@6 lint "contracts/openapi/*.yaml" \
		--ruleset .spectral.yaml --fail-severity=error

contracts-lint-asyncapi: ## AsyncAPI CLI
	npx --yes @asyncapi/cli@2 validate contracts/asyncapi/booking-events.yaml

## --- everything ----------------------------------------------------------

check: vet build test contracts-lint ## What CI runs
