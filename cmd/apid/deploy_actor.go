// Issue #606 / SAFE-RELEASES-E.1: per-deployment actor attribution.
// This file is the single source of truth for "given an incoming
// HTTP request that submitted a deployment, who is the actor?" —
// both the server-side column stamps (deployed_by_user_id,
// deployed_via, deployed_from_ip) and the audit-row actor string
// (s.audit.EmitAs(..., resolvedActor, ...)) route through the helpers
// here so the cookie-vs-bearer-vs-API-key classifier never forks
// across handler files.
//
// The classification is deterministic from the request shape: a
// session cookie present → dashboard; bearer token → CLI (the
// CLI stores its key as a bearer per pkg/gregale/auth); API-key
// header → api (machine-to-machine / SDK); the githubd_bridge path
// stamps "github" at the bridge, not here (the bridge doesn't carry
// an HTTP request — req.Pusher.Login is the resolved actor there).
//
// The closed-set vocabulary (api / cli / dashboard / github /
// operator) is enforced at the schema layer by
// migrations/00303_deployments_actor.sql's CHECK constraint — the
// helpers here return only values in that set.

package main

import (
	"net/http"

	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// routeKindForRequest returns the closed-set classifier for an
// inbound HTTP request. One of "api" / "cli" / "dashboard". The
// githubd_bridge path stamps "github" at the bridge itself
// (cmd/apid/githubd_bridge.go::EnqueueBuild) so this helper never
// has to inspect proto headers — handlers called via the bridge
// are not HTTP-routed.
//
// Classification order:
//  1. Has session cookie → "dashboard" (the dashboard UI sets the
//     cookie at /v1/auth/session-create — see handlers_issue_session.go).
//  2. Has Authorization: Bearer → "cli" (the CLI writes bearer tokens
//     per pkg/gregale/auth; the bearer header is the canonical
//     machine-to-machine shape from the CLI's pov).
//  3. Has X-API-Key → "api" (programmatic access via API key —
//     the SDK uses this path).
//  4. Else → "api" (default for unauthenticated-but-passing-the-
//     gateway paths; a future commit can tighten the default to
//     "operator" if observability shows the fallback is hit on
//     any customer surface).
//
// The helper is intentionally order-sensitive: a session cookie
// always wins over a bearer header (a CLI invocation that happens
// to carry a session cookie is a misconfiguration we want to
// attribute to the dashboard, not the CLI — that's what the
// session is for).
func routeKindForRequest(r *http.Request) string {
	if cookieSessionPresent(r) {
		return "dashboard"
	}
	if h := r.Header.Get("Authorization"); h != "" && len(h) >= 7 && (h[:7] == "Bearer " || h[:7] == "bearer ") {
		return "cli"
	}
	if r.Header.Get("X-API-Key") != "" {
		return "api"
	}
	return "api"
}

// cookieSessionPresent returns true if the request carries a
// session cookie (the dashboard path). Extracted so the
// classification doesn't bake cookie-name knowledge into two
// helpers — a future cookie rename touches one place.
//
// The cookie name is the same one handlers_issue_session.go writes
// ("faas_session"); a direct header lookup keeps the dependency
// graph lean (this file is referenced from every deploy handler).
func cookieSessionPresent(r *http.Request) bool {
	if _, err := r.Cookie("faas_session"); err == nil {
		return true
	}
	return false
}

// stampDeploymentActor stamps the three server-resolved actor
// columns onto a *state.Deployment before INSERT. acct.ID is the
// deploying account's UUID (always set — the handler ran
// loadAppAndPreflight which enforced auth). The IP comes from
// pkg/middleware.ClientIP so the trust contract matches the
// auth-limit bucket.
//
// PusherLogin is NOT stamped here — that path is bridge-only and
// lives in cmd/apid/githubd_bridge.go::EnqueueBuild.
//
// Empty fields are kept as "" on the struct; the pgstore INSERT
// path coalesces them to NULL/” via the migrations/00303 SQL
// shape (nullif() + coalesce() chain).
func stampDeploymentActor(d *state.Deployment, acct state.Account, r *http.Request) {
	d.DeployedByUserID = acct.ID
	d.DeployedVia = routeKindForRequest(r)
	if ip := middleware.ClientIP(r); ip != "" {
		d.DeployedFromIP = ip
	}
}

// mergeActorAudit folds the actor context into an existing audit
// payload map. Mirrors the eventual PR #984 mergeAnnotationAudit
// pattern: zero-valued fields drop (no "actor_user_id": "" keys on
// the wire). Callers always pass a non-nil data map; nil handling
// is intentionally omitted (all existing audit Emit call sites
// allocate the map inline at the Emit call site).
//
// Concatenated keys:
//   - actor_user_id (string)
//   - actor_via     (closed-set string)
//   - actor_ip      (string; empty when request had no trusted IP)
//   - actor_pusher  (string; non-empty only on githubd_bridge path)
//
// The "omit when zero" rule matches PR #984's annotation-merge
// helper so the audit row shape stays grep-friendly across PRs.
func mergeActorAudit(data map[string]any, userID, via, ip, pusher string) map[string]any {
	if data == nil {
		data = map[string]any{}
	}
	if userID != "" {
		data["actor_user_id"] = userID
	}
	if via != "" {
		data["actor_via"] = via
	}
	if ip != "" {
		data["actor_ip"] = ip
	}
	if pusher != "" {
		data["actor_pusher"] = pusher
	}
	return data
}

// resolvedActorString returns the canonical `<via>:<id>` form for
// s.audit.EmitAs. Used by commit 3 to widen the audit row's actor
// column from the constructor-baked "apid" to the per-call resolved
// actor (PR-E1.3). The shape mirrors the metric-labelling convention
// in pkg/audit (free-form, daemons-as-namespaces).
//
// userID is the deploying account's UUID (or empty for githubd
// path where the pusher isn't a local account). pusher is the raw
// GH login for the githubd path; empty otherwise.
//
// Examples:
//
//	resolvedActorString("dashboard", "8a2...", "", "") = "dashboard:8a2..."
//	resolvedActorString("github",    "",     "", "poyrazK") = "github:poyrazK"
//	resolvedActorString("cli",       "8a2...", "", "") = "cli:8a2..."
func resolvedActorString(via, userID, pusher string) string {
	switch via {
	case "github":
		if pusher != "" {
			return "github:" + pusher
		}
		// Fall through: a githubd call with no pusher (shouldn't
		// happen but the schema permits it) → attribute to the
		// bridge itself.
		return "github:unknown"
	default:
		if userID != "" {
			return via + ":" + userID
		}
		return via + ":unknown"
	}
}
