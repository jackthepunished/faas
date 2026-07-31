// Package reconcile drives Phase 5 repo decomposition: it observes
// a fresh reposcan.Result against an existing project and reconciles
// the project membership (apps + crons) to the scan in one Tx-safe
// pass. Three guards run BEFORE any write:
//
//  1. Never reconcile to empty — a successful scan that finds no
//     workloads means the customer removed every compose/procfile/
//     etc. file. Alert and bail.
//  2. Production branch only — pushes to a feature branch must NOT
//     change membership. Return Result.WasIgnored=true so the
//     caller can return 200-ignored.
//  3. Scan-source stability — a downgrade (compose → single) is
//     rejected via state.SetProjectScanSource + ErrScanSourceDowngrade.
//
// Each create/update/remove emits a project.workload.* audit row.
// The diff key is reposcan.Workload.Key() = (RootDir, Name). The
// package ships dormant on main: cmd/apid's scan_service.go picks
// it up in PR-G, cmd/githubd in PR-H.
package reconcile

import (
	"context"
	"io/fs"
	"log/slog"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/audit"
	"github.com/onebox-faas/faas/pkg/reposcan"
	"github.com/onebox-faas/faas/pkg/state"
)

// Service is the reconcile orchestrator. Construct one per apid or
// githubd process; the struct is safe to call from multiple goroutines
// only when Store, Audit, and Limits are themselves goroutine-safe
// (true for *state.PgStore and *audit.Auditor in production).
type Service struct {
	// Store is the state-layer facade. PgStore / MemStore both
	// implement the interface.
	Store state.Store
	// Audit is the canonical pkg/audit.Auditor — emits best-effort
	// audit rows via pkg/audit/audit.go:110-126 (no error return).
	// Constructed once per daemon with audit.New(store, log, ops,
	// "reconcile"); the actor is "reconcile" so dashboards can
	// distinguish reconcile-driven events from apid/githubd
	// handler-driven events.
	Audit *audit.Auditor
	// Limits resolves the per-plan quota table. Defaults to
	// api.MustLimitsFor in NewService; tests inject a fake.
	Limits func(plan api.Plan) api.Limits
	// Scan is the reposcan entry point. Defaults to reposcan.Scan
	// in NewService; tests inject a fake that returns a fixed
	// Result.
	Scan func(fsys fs.FS) (reposcan.Result, error)
	// Log receives diagnostic output. Optional; defaults to
	// slog.Default().
	Log *slog.Logger
}

// Result carries the outcome of a single Reconcile call. Members
// are zero unless populated by their corresponding action; callers
// should range over the slices and switch on the boolean flags.
type Result struct {
	// Added carries the new app rows created during this reconcile.
	Added []state.App
	// Removed carries the IDs of apps that were soft-deleted (status
	// flipped to AppDeleted per PR-E user decision).
	Removed []string
	// Changed carries the updated rows. The store returns the
	// post-update App from UpdateApp; diff context is in the
	// corresponding project.workload.changed audit row.
	Changed []state.App
	// BuildIDs is always nil in this mega-PR. PR-H fills it via
	// the path-filtered fan-out (push commit → which workload
	// changed → which build to enqueue).
	BuildIDs []string
	// Alerts carries guard-tripped notifications. Each Alert has a
	// stable Kind string ("no_workloads" | "scan_source_downgrade" |
	// "quota_blocked" | "feature_branch") so dashboards can group.
	Alerts []Alert
	// WasIgnored is true when guard #2 (production-branch-only)
	// tripped. The caller returns 200-ignored to the webhook.
	WasIgnored bool
}

// Alert is one entry in Result.Alerts. Kind is the stable enum;
// Message is a human-readable summary; Data is the structured
// payload that was also written to the audit row.
type Alert struct {
	Kind    string
	Message string
	Data    map[string]any
}

// NewService builds a Service with the default Scan and Limits
// resolvers. Callers typically set Store + Audit explicitly; the
// rest is wired by the constructor.
func NewService(store state.Store, aud *audit.Auditor, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		Store:  store,
		Audit:  aud,
		Limits: api.MustLimitsFor,
		Scan:   reposcan.Scan,
		Log:    log,
	}
}

// Reconcile is the single entry point. It is the package's only
// exported method. See package doc for the guard order and the
// audit/kind taxonomy.
//
// commitSHA + branch are part of the audit payload and the
// production-branch guard respectively. Branch is empty for
// interactive apid scan-and-apply (where the production-branch
// invariant is enforced upstream by the apid handler — every
// apid-driven reconcile hits the production branch by
// construction). githubd passes the actual pushed branch in
// PR-H.
func (s *Service) Reconcile(
	ctx context.Context,
	project state.Project,
	scan reposcan.Result,
	commitSHA string,
	branch string,
) (Result, error) {
	// Implementation lives in the reconcile_internal.go file in
	// the same package. Splitting it keeps this file readable as
	// the package's public-surface contract.
	return s.reconcile(ctx, project, scan, commitSHA, branch)
}
