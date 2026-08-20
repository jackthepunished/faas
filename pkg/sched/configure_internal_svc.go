package sched

// configure_internal_svc.go — ADR-119 wiring surface for
// outbound Authorization: Bearer JWTs on synth requests
// (pkg/sched/loop.go::httpGatewaySynth.SynthesizeRequest
// + Invoke + Loop.postBatch). The companion minter lives
// in cmd/schedd/internal_svc_minter.go (cmd-side closure over
// the env-loaded Ed25519 keypair).
//
// Why a configure-helper here instead of exposing the
// httpGatewaySynth type: the cmd-side imports the sched
// package as a black-box client (DialGatewaySynthTarget
// returns GatewaySynth, an interface). Exposing the
// concrete type just so cmd can call WithAppPublicAuthModeLookup
// / WithMintInternalSvcToken would invert the abstraction.
//
// ConfigureInternalSvcAuth type-asserts the GatewaySynth to
// the concrete *httpGatewaySynth and wires both setters in
// one call. The assertion is nil-safe (the function no-ops
// when the underlying impl isn't httpGatewaySynth — e.g. a
// future in-memory fake used by a unit test).

import (
	"context"
	"errors"

	"github.com/onebox-faas/faas/pkg/state"
)

// PublicAuthModeLookupResult is the structured return of
// PublicAuthModeLookupFunc. Mode is the apps.public_auth_mode
// value (open | bearer | basic | ip_allowlist | internal_only).
// Err is non-nil when the lookup failed — a transient DB
// outage, connection error, or any other store failure.
//
// The fail-closed posture (ADR-119 round-2 follow-up):
// callers (SynthesizeRequest, Invoke, postBatch) treat any
// non-nil Err as "the app is in internal_only mode" — i.e.
// return AuthModeAssumeInternalOnly. Without this, a transient
// PG outage during a cron tick to an internal_only app would
// return Mode == "" (open), schedd would omit the JWT, and
// the gateway-side gate would also return "" on the same
// error → invoke succeeds without auth. That is a fail-open
// path. Code review #23 finding #3.
//
// The sentinel is exported so callers can branch on
// errors.Is(err, ErrAuthModeLookup) if they want a different
// posture (e.g. a future hardening pass that aborts the
// request entirely on lookup failure rather than assuming
// internal_only).
var ErrAuthModeLookup = errors.New("sched: public_auth_mode lookup failed")

// PublicAuthModeLookupResult captures the lookup outcome. Mode
// is meaningful only when Err == nil; on Err != nil the caller
// should use the sentinel path (or fall back to assuming
// internal_only).
type PublicAuthModeLookupResult struct {
	Mode string
	Err  error
}

// PublicAuthModeLookupFunc is the cmd-side mode-lookup
// signature sched/httpGatewaySynth.{SynthesizeRequest,Invoke}
// + Loop.postBatch consult. Returns the per-app
// public_auth_mode plus a non-nil error when the lookup
// failed. The cmd-side wiring (cmd/schedd/main.go) constructs
// the closure over l.engine.Store().AppByID.
//
// The ctx plumbed through here is the caller's ctx (cron
// tick, drain, trigger dispatch) — not a fresh
// context.Background(). The previous shape used
// context.Background() which made the lookup unaffected by
// caller-side cancellation (the cron tick's deadline, the
// schedd's shutdown signal, etc.). After code review #23
// finding #4 we thread the caller ctx end-to-end so the
// goroutine honours shutdown deadlines.
type PublicAuthModeLookupFunc func(ctx context.Context, appID string) (PublicAuthModeLookupResult, error)

// InternalSvcMintFunc is the cmd-side mint closure signature.
// Receives appID so the JWT can carry an app_id claim for
// future per-app key-pinning. Returns the signed JWT or an
// error from pkg/internalsvc.Mint (signing-key size, JSON
// marshal failure, etc.).
type InternalSvcMintFunc func(appID string) (string, error)

// ConfigureInternalSvcAuth (ADR-119) wires the per-app
// public_auth_mode lookup + JWT minter into the given
// GatewaySynth. Both fields are nil-safe individually
// (SynthesizeRequest checks for nil at call time); the
// helper exists so the cmd-side wiring is one call rather
// than a type-assert + two setters. Returns true when the
// wiring succeeded (the underlying impl was httpGatewaySynth);
// returns false silently when the underlying impl is some
// other GatewaySynth (e.g. a unit-test fake) — the caller
// typically doesn't care because SynthesizeRequest on those
// fakes doesn't go through HTTP at all.
func ConfigureInternalSvcAuth(g GatewaySynth, modeLookup PublicAuthModeLookupFunc, mint InternalSvcMintFunc) bool {
	hg, ok := g.(*httpGatewaySynth)
	if !ok {
		return false
	}
	hg.WithAppPublicAuthModeLookup(modeLookup)
	hg.WithMintInternalSvcToken(mint)
	return true
}

// PublicAuthModeFromStore constructs a PublicAuthModeLookupFunc
// closure over the schedd engine Store. The closure reads
// apps.public_auth_mode on every call (rare — cron cadence)
// using the caller-provided ctx. Returns ErrAuthModeLookup
// on any error — never silently returns "" (which would be a
// fail-open posture; see the round-2 follow-up note at the
// top of this file).
//
// The interface parameter is loose (only AppByID is named) so
// the cmd-side wiring doesn't need to import the full
// pkg/sched.Engine surface. A unit test can construct a
// minimal fake that satisfies the signature.
func PublicAuthModeFromStore(lookup func(ctx context.Context, appID string) (state.App, error)) PublicAuthModeLookupFunc {
	return func(ctx context.Context, appID string) (PublicAuthModeLookupResult, error) {
		app, err := lookup(ctx, appID)
		if err != nil {
			return PublicAuthModeLookupResult{}, ErrAuthModeLookup
		}
		return PublicAuthModeLookupResult{Mode: app.PublicAuthMode}, nil
	}
}