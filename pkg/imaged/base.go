package imaged

// Base image references per runtime (spec §4.6 two-drive scheme, ADR-005).
//
// imaged.handleDeployment pulls the app's manifest, then pulls the matching
// base's manifest to learn the base's diff_ids, then computes LayersAboveBase.
// The base itself is NOT downloaded — drive0 is the shared read-only ext4
// produced from the base image, already on disk at /srv/fc/base/<runtime>.ext4.
//
// The defaults below match images/runner-node22.Dockerfile,
// images/runner-python312.Dockerfile, and images/base-minimal.Dockerfile on
// HEAD of main. They can be overridden at startup via config (cmd/imaged's
// TOML) so the box can roll a base image ahead of pinned refs and have imaged
// track it without a code change.
const (
	BaseRefNode22      = "ghcr.io/onebox-faas/runner-node22:latest"
	BaseRefPython312   = "ghcr.io/onebox-faas/runner-python312:latest"
	BaseRefGo124       = "ghcr.io/onebox-faas/runner-go124:latest"
	BaseRefGo124Alpine = "ghcr.io/onebox-faas/runner-go124-alpine:latest"
	BaseRefMinimal     = "ghcr.io/onebox-faas/base-minimal:latest"
	BaseRefBuilder     = "ghcr.io/onebox-faas/builder-base:latest"

	// Runtime names are the values stored on state.App.Runtime. They map
	// 1:1 to the runner shims in guest/runners/{node22,python312,go124}.
	// go124-alpine reuses the go124 runner shim against a musl base
	// (images/runner-go124-alpine.Dockerfile); libc only differs.
	// Naming them as constants keeps the baseRefFor switch and the
	// production callers (cmd/imaged's deploy path) in lockstep.
	RuntimeNode22      = "node22"
	RuntimePython312   = "python312"
	RuntimeGo124       = "go124"
	RuntimeGo124Alpine = "go124-alpine"
)

// baseRefFor returns the canonical base image reference for a runtime. The
// empty runtime maps to the minimal base (plain apps, spec §4.6).
//
// go124-alpine is opt-in: customers who need the musl base set
// runtime=go124-alpine explicitly. The default go124 base stays
// bookworm (glibc) so existing deploys see no behavior change. A
// future PR may flip the default after measuring fleet-wide
// snapshot_fleet_avg_mb with both bases co-resident
// (pkg/api/limits.go::FleetSnapshotAvgTargetMB = 130, alarm 160).
func baseRefFor(runtime string) string {
	switch runtime {
	case RuntimeNode22:
		return BaseRefNode22
	case RuntimePython312:
		return BaseRefPython312
	case RuntimeGo124:
		return BaseRefGo124
	case RuntimeGo124Alpine:
		return BaseRefGo124Alpine
	default:
		return BaseRefMinimal
	}
}
