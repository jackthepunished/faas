// handlers_admin_force_park.go — operator-side recovery primitive
// P2a (force-park). The on-call engineer posts to this endpoint
// when an instance is wedged in {RUNNING, WAKING, COLD_BOOTING}
// and the customer can't wait for the idle reaper. The handler
// routes through schedd's existing ParkInstance RPC
// (pkg/scheddgrpc.Server.ParkInstance) so the state-machine
// guard at pkg/state/machine.go:88-95 fires — schedd is the
// ONLY writer to `instances` per CLAUDE.md §6.2. A direct
// apid→store write would bypass lockApp + CanTransition and
// risk a state-machine violation.
//
// Auth + IDOR posture mirrors getAppMetrics (admin scope + MFA +
// s.adminAllows). Requires ?confirm=true as a tripwire — same
// pattern as /v1/compute-nodes/{name}/force-drain's --yes ack
// (commands_compute_nodes.go:249). Without confirm the handler
// returns 400 validation_failed so an operator can't fat-finger
// the call.
//
// Reason validation: a-z, 0-9, underscore only, ≤64 chars. The
// shape matches the audit log's parser at pkg/audit (which
// expects a slug-like token); non-conforming reasons are rejected
// with 400. Reason defaults to "operator_force_park" when the
// caller omits ?reason=.
//
// Audit row: operator.action.park_instance with
//
//	account_id = target instance's account id
//	data = {actor: caller.ID, instance_id, app_id, deployment_id,
//	        previous_state, reason, schedd_result}
//
// previous_state reflects the gate-time read (NOT the post-call
// state) so a parallel customer-driven Park race doesn't lie in
// the audit log — same pattern as handlers_admin_obs.go.
package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// maxForceParkReasonLen caps the ?reason= query param so the
// audit row's data JSON stays bounded (matches the conventions at
// handlers_audit.go:255).
const maxForceParkReasonLen = 64

// forceParkReasonShape is the closed character class for
// ?reason=. a-z, 0-9, underscore only — same shape as the
// audit log's parser expects at pkg/audit. Other characters
// (whitespace, punctuation, emoji) are rejected at the handler
// so an operator typo doesn't survive into the audit log.
var forceParkReasonShape = regexp.MustCompile(`^[a-z0-9_]*$`)

// forceParkableStates is the closed set of instance states from
// which a force-park is allowed. Anything else (PARKED, PARKED_
// SCHEDULED, PARKED_FAILED, STOPPED, DELETED, …) returns 409
// instance_not_parkable. The list is intentionally conservative:
// if the instance is already parked, we don't want to spam the
// audit log with no-op rows.
//
// The map is keyed by the raw instance.state string (the
// `instances.state` column is a plain text column — see
// migrations/00015) rather than a typed alias, because Go's
// pkg/state does not export a State type today (the canonical
// constants live unexported in memstore.go). String comparison
// against the raw column value is the same shape the state
// machine at pkg/state/machine.go:88-95 uses internally.
var forceParkableStates = map[string]struct{}{
	"RUNNING":      {},
	"WAKING":       {},
	"COLD_BOOTING": {},
}

// forceRecoverer is the small surface apid calls into schedd for
// the P2 recovery primitives. Defining it here (rather than
// depending on the full *scheddgrpc.Client) lets the handler
// tests substitute a hand-rolled fake without spinning up a
// gRPC server. The production type — *scheddgrpc.Client —
// satisfies this interface via its ParkInstance and
// ForceColdBootNextWake methods.
type forceRecoverer interface {
	ParkInstance(ctx context.Context, instanceID, reason string) error
	ForceColdBootNextWake(ctx context.Context, deploymentID string) ([]string, error)
}

// postForcePark handles POST /v1/admin/instances/{id}/force-park.
// 200 on success, 400 on missing ?confirm=true or invalid
// ?reason=, 403 admin_required, 404 instance_not_found, 409
// instance_not_parkable, 503 when scheddClient is not wired.
func (s *server) postForcePark(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if s.scheddClient == nil {
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable,
			"schedd_unavailable",
			"schedd client not wired",
			"FAAS_SCHEDD_SOCKET is empty on this deployment; admin recovery endpoints are unreachable"))
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"confirm required",
			"?confirm=true is required to force-park an instance; aborts on operator typo"))
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "operator_force_park"
	}
	if len(reason) > maxForceParkReasonLen || !forceParkReasonShape.MatchString(reason) {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"invalid reason",
			"reason must match [a-z0-9_]{1,64}"))
		return
	}

	instanceID := r.PathValue("id")
	if _, perr := uuid.Parse(instanceID); perr != nil {
		api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
			"instance not found", "instance id is not a valid uuid"))
		return
	}

	// State gate: read the instance fresh so the gate-time check
	// matches the audit row's previous_state. A racing customer-
	// driven Park can move the row between this read and the
	// schedd RPC — the handler doc-comment documents the race.
	ins, err := s.store.InstanceByID(r.Context(), instanceID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"instance not found", "no instance row with that id"))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusInternalServerError,
			api.CodeInternal, "instance read failed", err.Error()))
		return
	}
	if _, ok := forceParkableStates[ins.State]; !ok {
		emitOperatorActionParkInstance(r, s, acct, "", instanceID,
			ins.AppID, ins.DeploymentID, ins.State, reason, "error")
		api.WriteProblem(w, api.NewProblem(http.StatusConflict,
			"instance_not_parkable",
			"instance is not in a parkable state",
			"current state: "+ins.State))
		return
	}

	// Resolve the app → account so the audit row's account_id is
	// the instance's owning account (not the calling admin's
	// account). If app resolution fails we still emit the audit
	// row with targetAccountID="" — the audit row is durable
	// even when the join is impossible (mirrors the precedent at
	// emitOperatorActionForceColdBoot which also tolerates a
	// missing app row).
	app, aerr := s.store.AppByID(r.Context(), ins.AppID)
	targetAccountID := ""
	if aerr == nil {
		targetAccountID = app.AccountID
	}

	parkErr := s.scheddClient.ParkInstance(r.Context(), instanceID, reason)
	result := "success"
	if parkErr != nil {
		result = "error"
	}
	emitOperatorActionParkInstance(r, s, acct, targetAccountID, instanceID,
		ins.AppID, ins.DeploymentID, ins.State, reason, result)
	if parkErr != nil {
		if errors.Is(parkErr, state.ErrNotFound) {
			api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
				"instance not found", "schedd could not find the instance"))
			return
		}
		api.WriteProblem(w, api.NewProblem(http.StatusServiceUnavailable,
			"schedd_unavailable", "park RPC failed", parkErr.Error()))
		return
	}
	writeJSON(w, http.StatusOK, api.ForceParkResponse{
		OK:            true,
		InstanceID:    instanceID,
		PreviousState: ins.State,
		Reason:        reason,
	})
}
