module github.com/morisempai/wakewake/services/notification

go 1.26.0

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/morisempai/wakewake/shared/contracts v0.0.0
	github.com/morisempai/wakewake/shared/platform v0.0.0-00010101000000-000000000000
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/rabbitmq/amqp091-go v1.12.0 // indirect
	go.opentelemetry.io/otel v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/text v0.32.0 // indirect
)

// shared/ modules are never published or tagged — they are internal to this monorepo. The
// replace directives make non-workspace builds work (GOWORK=off, and any Docker stage that
// copies only the modules it needs), so builds do not depend on go.work being present.
//
// shared/testkit is in replace but NOT require: only the integration-tagged tests import it, so
// go mod tidy (default tags) never pulls it — and its testcontainers dependency tree stays out
// of this service's go.sum. Under the workspace, -tags integration resolves it locally.
replace (
	github.com/morisempai/wakewake/shared/contracts => ../../shared/contracts
	github.com/morisempai/wakewake/shared/platform => ../../shared/platform
	github.com/morisempai/wakewake/shared/testkit => ../../shared/testkit
)
