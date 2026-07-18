module github.com/morisempai/wakewake/services/availability

go 1.26.0

require github.com/morisempai/wakewake/shared/contracts v0.0.0

require github.com/google/uuid v1.6.0 // indirect

// shared/ modules are never published or tagged — they are internal to this monorepo. The
// replace directives make non-workspace builds work (GOWORK=off, and any Docker stage that
// copies only the modules it needs), so builds do not depend on go.work being present.
replace (
	github.com/morisempai/wakewake/shared/contracts => ../../shared/contracts
	github.com/morisempai/wakewake/shared/platform => ../../shared/platform
	github.com/morisempai/wakewake/shared/testkit => ../../shared/testkit
)
