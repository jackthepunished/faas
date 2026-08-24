package reconcile

// audit_kind_scope_excluded_test.go — pins the load-bearing shape
// of KindProjectScopeExcluded (ADR-124 follow-up #3, migration
// 00418). The constant is referenced by:
//
//   - cmd/apid/scan_service.go's slog seam (preview-only; preview
//     does NOT emit this kind, only apply does — but the apply
//     path reuses scan_service's partition + gate, so the
//     constant must exist here)
//   - pkg/state/pgstore_deployment_scope_exclusions.go's audit
//     emission (commit 5 wires this up)
//   - dashboards / SOC 2 CC7.2 incident forensics queries
//     (kind=project.scope.excluded)
//
// The kind's project.scope.<verb> shape mirrors KindWorkloadSkipped
// (project.workload.<verb>) so the audit-events table group-by
// keeps the project.* closed set tidy. A refactor that flipped
// this to "project_scope_excluded" or "deployment.scope.excluded"
// would silently break the dashboard query and the alert rules;
// this test pins the value.
//
// This test does NOT exercise the audit emit (the emission path is
// wired up in PR-B commit 5 alongside --persist-exclude). It pins
// the constant alone so the kind is a stable compile-time
// contract that downstream callers can rely on before commit 5
// lands.

import (
	"strings"
	"testing"
)

// TestKindProjectScopeExcluded_PinsWireValue pins the kind's
// string value. A refactor that flipped it to a different shape
// would silently break dashboard queries keyed on
// `kind = "project.scope.excluded"`; this test catches that.
func TestKindProjectScopeExcluded_PinsWireValue(t *testing.T) {
	const want = "project.scope.excluded"
	if KindProjectScopeExcluded != want {
		t.Errorf("KindProjectScopeExcluded: got %q, want %q", KindProjectScopeExcluded, want)
	}
}

// TestKindProjectScopeExcluded_DistinctFromWorkloadSkipped pins
// the audit closed-set convention: project.workload.<verb> for
// per-scan operator decisions; project.scope.<verb> for
// persisted cross-deploy intent. Mixing the two would lose the
// SOC 2 CC7.2 paper trail distinction — "who skipped this on
// this scan" vs "who persistently excluded this across deploys".
func TestKindProjectScopeExcluded_DistinctFromWorkloadSkipped(t *testing.T) {
	if KindProjectScopeExcluded == KindWorkloadSkipped {
		t.Errorf("KindProjectScopeExcluded == KindWorkloadSkipped (%q) — closed-set violation; SOC 2 CC7.2 paper trail collapses",
			KindProjectScopeExcluded)
	}
}

// TestKindProjectScopeExcluded_PrefixedWithProject pins the kind
// naming convention. All audit kinds in this package start with
// `project.` (mirrors the KindReconcileStarted / KindWorkloadAdded
// / KindBuildEnqueued closed set). A future kind that drifted to
// a different prefix (e.g. `account.`) would break the dashboard
// group-by filter `kind LIKE 'project.%'`.
func TestKindProjectScopeExcluded_PrefixedWithProject(t *testing.T) {
	if !strings.HasPrefix(KindProjectScopeExcluded, "project.") {
		t.Errorf("KindProjectScopeExcluded %q: must start with %q (audit closed-set prefix)",
			KindProjectScopeExcluded, "project.")
	}
}