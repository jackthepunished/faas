// handlers_admin_force_cold_boot.go — operator-side recovery primitive
// P2b (force-cold-boot-next-wake). The on-call engineer posts here
// when the live instance looks fine but the snapshot backing the
// warm tier is suspected to be the carrier of a customer-reported
// wedge. Per ADR-005 ("snapshot of a wedged VM is a wedged VM"),
// the recovery action is to mark the latest warm + init snapshots
// stale — NOT to mutate the instance row. The next customer Wake
// then takes the cold-boot path through
// `engine.go:4800-4820 usableSnapshotForWake` returning
// `haveSnap=false`.
//
// Auth + IDOR posture mirrors postForcePark: admin scope + MFA +
// s.adminAllows (allowlist check), ?confirm=true tripwire. The
// handler resolves the slug → apps row → latest deployments row
// (by created_at DESC) and forwards the deployment ID to
// schedd's gRPC RPC.
//
// The handler also has to handle the case where the deployment
// has zero snapshots. Per the spec edge case "Empty": engine
// returns ([]string{}, nil) and the handler stamps the empty
// list in the audit row (durable record of operator check, even
// when no-op). The HTTP response is 200 OK in both cases.
//
// Race note: a customer Wake can race with this handler — the
// engine's MarkSnapshotStale is idempotent (re-stamp is a no-op
// success), and the next Wake still picks `haveSnap=false` from
// the post-stale read. Same precedent as the postForcePark race
// documentation.
package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// forceColdBootReasonShape is the same a-z0-9_ shape used for
// force-park. We accept the same reason vocabulary for both
// recovery primitives so the operator dashboard's filter chip
// strip is consistent. Reason is optional (defaults to
// "operator_force_cold_boot").
var forceColdBootReasonShape = regexp.MustCompile(`^[a-z0-9_]*$`)

// maxForceColdBootReasonLen mirrors maxForceParkReasonLen so
// the audit log row's data JSON stays bounded across both
// recovery primitives.
const maxForceColdBootReasonLen = 64

// latestDeploymentForApp returns the most-recent deployment row
// for an app, via Store.LatestDeployment (created_at DESC).
// Single-deployment choice documented in plan §P2b edge cases.
// The Store method's contract is "the app's latest deployment
// row ordered DESC by created_at" (mirrors
// pkg/state/pgstore.go:4435), and returns state.ErrNotFound
// when the app has no deployments.
func (s *server) latestDeploymentForApp(ctx context.Context, appID string) (state.Deployment, error) {
	return s.store.LatestDeployment(ctx, appID)
}

// postForceColdBoot handles POST /v1/admin/apps/{slug}/force-cold-boot.
// 200 on success (snap_ids_marked_stale may be empty), 400 on
// missing ?confirm=true or invalid ?reason=, 403 admin_required,
// 404 app_not_found or deployment_not_found, 503 when scheddClient
// is not wired.
func (s *server) postForceColdBoot(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if s.scheddClient == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable,
			"schedd_unavailable",
			"schedd client not wired",
			"FAAS_APID_SCHEDD_TARGET is empty on this deployment; admin recovery endpoints are unreachable"))
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"confirm required",
			"?confirm=true is required to force-cold-boot; aborts on operator typo"))
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "operator_force_cold_boot"
	}
	if len(reason) > maxForceColdBootReasonLen || !forceColdBootReasonShape.MatchString(reason) {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid reason",
			"reason must match [a-z0-9_]{1,64}"))
		return
	}

	slug := r.PathValue("slug")
	if slug == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid slug", "slug path segment is empty"))
		return
	}

	app, err := s.store.AppBySlug(r.Context(), slug)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"app not found", "no app with that slug"))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeInternal, "app read failed", err.Error()))
		return
	}

	dep, err := s.latestDeploymentForApp(r.Context(), app.ID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"deployment not found", "app has no deployments to force-cold-boot"))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeInternal, "deployment read failed", err.Error()))
		return
	}

	snapIDs, fcErr := s.scheddClient.ForceColdBootNextWake(r.Context(), dep.ID)
	emitOperatorActionForceColdBoot(r, s, acct, app.AccountID, app.ID, dep.ID, snapIDs)
	if fcErr != nil {
		if errors.Is(fcErr, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"deployment not found", "schedd could not find the deployment"))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable,
			"schedd_unavailable", "force-cold-boot RPC failed", fcErr.Error()))
		return
	}
	writeJSON(w, http.StatusOK, api.ForceColdBootResponse{
		OK:                 true,
		AppID:              app.ID,
		DeploymentID:       dep.ID,
		SnapIDsMarkedStale: snapIDs,
		Reason:             reason,
		TierWalked:         []string{"warm", "init"},
	})
}
