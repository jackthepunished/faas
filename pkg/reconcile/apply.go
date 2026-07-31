// Apply operations. Three concerns:
//   - quota re-check (per-project membership cap) BEFORE the
//     create set is sent to ApplyProjectPlan. The store re-checks
//     under the Tx; the pre-check is best-effort and emits the
//     audit row.
//   - StartCommand emission. The Pr-E widening accepts *string;
//     the empty-string-means-NULL convention is enforced by the
//     store's nullString helper, so apply.go just passes the
//     resolved string.
//   - removed BEFORE SoftDeleteAppCascade. ADR-035 best-effort +
//     Audit.Emit returns no error, so the audit row exists even
//     when the cascade SQL fails halfway. Order matters for the
//     audit-ordering test.

package reconcile

import (
	"context"
	"errors"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/reposcan"
	"github.com/onebox-faas/faas/pkg/state"
)

// applyActions executes the diff. Quota pre-check is done here so
// the audit row is emitted before any SQL runs. Updates and
// removes always run; creates run as a single ApplyProjectPlan Tx
// (the Phase 3 reuse — quota is re-checked under the transaction).
func (s *Service) applyActions(
	ctx context.Context,
	project state.Project,
	actions []Action,
	commitSHA string,
) (Result, error) {
	var out Result

	// Partition by op so we can run them in the documented order
	// (updates, removes, creates) and so the per-op quota + audit
	// logic is independent.
	var creates, updates, removes []Action
	for _, a := range actions {
		switch a.Op {
		case "create":
			creates = append(creates, a)
		case "update":
			updates = append(updates, a)
		case "remove":
			removes = append(removes, a)
		}
	}

	// 1. Resolve the authoritative plan from the account. The
	// project does not carry a plan today; the limits table is
	// keyed on Account.Plan (single source of truth for quotas).
	acct, err := s.Store.AccountByID(ctx, project.AccountID)
	if err != nil {
		return out, err
	}
	limits := s.Limits(acct.Plan)
	planCap := limits.DeployedApps
	if planCap == 0 {
		// Defensive fallback — the limits table always has a
		// row for a known plan; anything else is a programmer
		// error. The inner ApplyProjectPlan Tx still enforces
		// the authoritative cap.
		planCap = api.MustLimitsFor(api.PlanScale).DeployedApps
	}

	// 2. Updates first (no quota concern).
	for _, u := range updates {
		changed, err := s.applyUpdate(ctx, project, u)
		if err != nil {
			return out, err
		}
		if changed != nil {
			out.Changed = append(out.Changed, *changed)
		}
	}

	// 3. Removes next. Emit the audit row BEFORE the cascade so
	// the audit-ordering test pins the order.
	for _, r := range removes {
		if err := s.applyRemove(ctx, project, r, commitSHA); err != nil {
			return out, err
		}
		out.Removed = append(out.Removed, r.App.ID)
	}

	// 4. Creates last, as a single Tx via the Phase 3 reuse.
	// Empty create set is a no-op.
	if len(creates) == 0 {
		return out, nil
	}
	// Member-of quota pre-check. DeployedApps is the per-plan
	// cap. The pre-check is best-effort; the inner Tx re-checks.
	existingApps, err := s.Store.AppsForProject(ctx, project.AccountID, project.ID)
	if err != nil {
		return out, err
	}
	projected := len(existingApps) + len(creates)
	if projected > planCap {
		skipped := make([]string, 0, len(creates))
		for _, c := range creates {
			skipped = append(skipped, c.Workload.Name)
		}
		s.emitReconcileQuotaBlocked(
			ctx, project,
			planCap,
			len(existingApps),
			len(creates),
			skipped,
			commitSHA,
		)
		out.Alerts = append(out.Alerts, Alert{
			Kind:    AlertKindQuotaBlocked,
			Message: "create set would exceed deployed_apps quota",
			Data: map[string]any{
				"limit":           planCap,
				"observed":        len(existingApps),
				"wouldbe_count":   len(creates),
				"skipped_creates": skipped,
			},
		})
		// Updates + removes already applied. Bail here.
		return out, nil
	}

	// Build the store.App slice for the create set. CreateAppIfUnderQuota
	// is the per-app quota-aware insert path (memstore.go:1286 /
	// pgstore.go:1090). It does NOT touch the project row — the
	// project already exists when reconcile runs (the create-project
	// Tx is the apid path, not the reconcile path).
	newApps := make([]state.App, 0, len(creates))
	for _, c := range creates {
		app := workloadToDraftApp(project, c.Workload, c.StartCommand)
		newApps = append(newApps, app)
	}
	added := make([]state.App, 0, len(newApps))
	for _, a := range newApps {
		created, err := s.Store.CreateAppIfUnderQuota(ctx, a, limits)
		if err != nil {
			// If the inner call raised a QuotaError, convert to
			// an alert and return (so the caller's behavioral
			// contract is consistent with the pre-check path).
			var qe *state.QuotaError
			if errors.As(err, &qe) {
				// Emit ONE quota_blocked audit row covering the
				// full create set (the names of the NOT-yet-added
				// members). Subsequent quota-blocked creates are
				// not retried: the caller must re-run reconcile
				// after a lift to add them.
				skipped := namesFromActions(creates)
				s.emitReconcileQuotaBlocked(
					ctx, project,
					qe.Limit,
					qe.Observed,
					len(creates),
					skipped,
					commitSHA,
				)
				out.Alerts = append(out.Alerts, Alert{
					Kind:    AlertKindQuotaBlocked,
					Message: "create set rejected by store quota",
					Data: map[string]any{
						"limit":           qe.Limit,
						"observed":        qe.Observed,
						"wouldbe_count":   len(creates),
						"skipped_creates": skipped,
					},
				})
				out.Added = added
				return out, nil
			}
			return out, err
		}
		added = append(added, created)
	}
	out.Added = added
	for _, a := range added {
		s.emitWorkloadAdded(ctx, project, a, commitSHA)
	}
	return out, nil
}

// applyUpdate runs a single UpdateApp. Returns the post-update
// App (or nil if the update was a no-op), and an error.
func (s *Service) applyUpdate(
	ctx context.Context,
	project state.Project,
	a Action,
) (*state.App, error) {
	rootDir := a.Workload.RootDir
	workloadName := a.Workload.Name
	params := state.UpdateAppParams{
		RootDir:      &rootDir,
		WorkloadName: &workloadName,
		StartCommand: &a.StartCommand,
	}
	updated, err := s.Store.UpdateApp(ctx, a.App.ID, params)
	if err != nil {
		return nil, err
	}
	s.emitWorkloadChanged(ctx, project, updated, a.fieldsChanged, "")
	return &updated, nil
}

// applyRemove runs a single SoftDeleteAppCascade. The audit row
// is emitted BEFORE the SQL so the audit-ordering test pins the
// order. The cascading behaviour is status-only per the PR-E
// user decision; envs, crons, custom_domains survive.
func (s *Service) applyRemove(
	ctx context.Context,
	project state.Project,
	a Action,
	commitSHA string,
) error {
	s.emitWorkloadRemoved(ctx, project, a.App.ID, a.App.WorkloadName, commitSHA)
	_, err := s.Store.SoftDeleteAppCascade(ctx, a.App.ID)
	return err
}

// workloadToDraftApp mirrors cmd/apid/scan_service.go:494-525
// (wlToApp) — kept in sync via the unit test. The Project.IsZero
// case is filtered out upstream; this helper assumes a real
// project. The store sets id, created_at, status, and backfills
// project_id on insert.
func workloadToDraftApp(project state.Project, w reposcan.Workload, startCmd string) state.App {
	class := state.WorkloadClass(string(w.Class))
	if class == "" {
		class = state.WorkloadClassHTTP
	}
	if w.Class == reposcan.ClassServer {
		// "server" hint is normalised to "http" — ADR-051 will
		// re-derive the authoritative class.
		class = state.WorkloadClassHTTP
	}
	return state.App{
		AccountID:     project.AccountID,
		ProjectID:     project.ID,
		Slug:          slugify(w.Name),
		RootDir:       w.RootDir,
		WorkloadName:  w.Name,
		WorkloadClass: class,
		StartCommand:  startCmd,
	}
}

// slugify mirrors the apid path. Kept as a small helper here
// rather than the pkg/state package so pkg/reconcile stays
// vendorable by cmd/githubd without a state-package widening.
func slugify(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == ' ' || c == '-' || c == '_':
			out = append(out, '-')
		}
	}
	return string(out)
}

// namesFromActions extracts the workload_name from each action in
// the create set. Used by the quota_blocked audit row.
func namesFromActions(actions []Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Workload.Name)
	}
	return out
}
