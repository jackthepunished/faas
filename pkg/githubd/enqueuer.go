// Build-enqueuer seam for githubd's push-dispatch path (PR-GH.5,
// repo decomposition Phase 5).
//
// PR-GH.4 wired the push-dispatch path through
// pkg/reconcile.Service so a push reconciles the project
// (apps rows added/changed/removed). PR-GH.5 fans the reconcile
// result out into per-app builds: every app in Result.Added ∪
// Result.Changed gets a build enqueued via the BuildEnqueuer
// seam.
//
// FAN-OUT SCOPE (intentional, addressed in a follow-up):
//
// The current implementation is a naive FULL fan-out: every
// touched app is rebuilt regardless of which files changed.
// This is correct (correct = "rebuild what the user pushed to")
// but conservative. A path-filtered follow-up (compare/{base}
// ...{head} per push, only rebuild apps whose RootDir changed
// files) is deferred per ADR-050 — the loader fan-out
// throughput is enough for v1.0 volumes. A TODO at the
// call site points at the ADR-050 reference.
//
// RETRY + PARTIAL-SUCCESS:
// Enqueue failures are best-effort (logged, not returned).
// The webhooks HTTP contract is "200 OK with deployment_id"
// on success; partial-success is rendered as
// {reconciled: true, build_ids: [...]} with a separate
// metric counter for enqueue failures. The alternative
// (failing the entire push because one of 50 builds was
// rejected) is worse for the customer.

package githubd

import (
	"context"
	"log/slog"
)

// BuildEnqueuer is the seam githubd uses to schedule a build
// for one (app, commit) pair. The production implementation
// will eventually call into builderd (or schedd's job queue)
// — for v1.0 the seam is satisfied by a noopEnqueuer that
// returns a synthetic build_id so the wire contract is
// exercised end-to-end. The follow-up slice that wires the
// real enqueue target will swap the implementation in
// cmd/githubd/main.go without touching this interface.
type BuildEnqueuer interface {
	// Enqueue schedules a build for the given app at the
	// given commit. The returned buildID is opaque to the
	// caller (the webhook response includes it as a string).
	// Errors are returned to the caller — the caller decides
	// whether to fail the push or treat the missing build as
	// a soft failure.
	Enqueue(ctx context.Context, accountID, appID, commitSHA string) (buildID string, err error)
}

// noopEnqueuer is the test + slice-9 default. It mints a
// deterministic buildID from the (appID, commitSHA) pair so
// the wire contract is pin-able without a real builder
// backend. PR-GH.5 swaps this for the real builderd bridge
// in a follow-up slice.
type noopEnqueuer struct {
	log *slog.Logger
}

// NewNoopEnqueuer returns a BuildEnqueuer that mints a
// synthetic buildID per call. The ID is the same for repeat
// (appID, commitSHA) within a single daemon lifetime
// (handy for log correlation), but it is NOT persistent —
// a daemon restart renumbers all builds.
func NewNoopEnqueuer(log *slog.Logger) BuildEnqueuer {
	if log == nil {
		log = slog.Default()
	}
	return &noopEnqueuer{log: log}
}

// Enqueue mints the synthetic buildID. The composition uses
// appID + commitSHA so two pushes to the same app produce
// different buildIDs (the per-app build history is then
// monotonic in commitSHA order).
func (n *noopEnqueuer) Enqueue(ctx context.Context, accountID, appID, commitSHA string) (string, error) {
	// Compose a synthetic ID. We deliberately do NOT use
	// uuid.NewV7 so this stays deterministic across reruns
	// (the unit tests pin specific IDs).
	bid := "build-" + appID + "-" + commitSHA
	n.log.Info("noop enqueuer: synthetic build",
		"build_id", bid, "app_id", appID, "commit_sha", commitSHA, "account_id", accountID)
	return bid, nil
}
