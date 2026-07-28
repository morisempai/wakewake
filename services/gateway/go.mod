module github.com/morisempai/wakewake/services/gateway

go 1.26.0

require (
	github.com/MicahParks/jwkset v0.11.0
	github.com/MicahParks/keyfunc/v3 v3.8.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/morisempai/wakewake/shared/platform v0.0.0
	golang.org/x/time v0.15.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
)

// shared/ modules are never published or tagged — they are internal to this monorepo. The replace
// directives make non-workspace builds work (GOWORK=off, and the Docker stage that copies only the
// modules it needs), so builds do not depend on go.work being present. shared/platform itself
// requires shared/contracts, so this main module must redeclare that replace for the build graph to
// resolve.
replace (
	github.com/morisempai/wakewake/shared/contracts => ../../shared/contracts
	github.com/morisempai/wakewake/shared/platform => ../../shared/platform
)
