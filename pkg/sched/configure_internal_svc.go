package sched

// configure_internal_svc.go — ADR-119 wiring surface for
// outbound Authorization: Bearer JWTs on synth requests
// (pkg/sched/loop.go::httpGatewaySynth.SynthesizeRequest). The
// companion minter lives in cmd/schedd/internal_svc_minter.go
// (cmd-side closure over the env-loaded Ed25519 keypair).
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

	"github.com/onebox-faas/faas/pkg/state"
)

// PublicAuthModeLookupFunc is the cmd-side mode-lookup
// signature sched/httpGatewaySynth.SynthesizeRequest expects.
// Returns the public_auth_mode for the given appID, or ""
// when the app isn't found (which SynthesizeRequest treats as
// "open" — no Authorization header attached). The cmd-side
// wiring (cmd/schedd/main.go) constructs the closure over
// l.engine.Store().AppByID.
type PublicAuthModeLookupFunc func(appID string) string

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
// and returns "" when the app isn't found. Cache miss
// returns "" — a future hardening pass could plumb a real
// error path so a stale-cache request fails rather than
// silently proceeds with the wrong auth state.
//
// The interface parameter is loose (only AppByID is named) so
// the cmd-side wiring doesn't need to import the full
// pkg/sched.Engine surface. A unit test can construct a
// minimal fake that satisfies the signature.
func PublicAuthModeFromStore(lookup func(ctx context.Context, appID string) (state.App, error)) PublicAuthModeLookupFunc {
	return func(appID string) string {
		app, err := lookup(context.Background(), appID)
		if err != nil {
			return ""
		}
		return app.PublicAuthMode
	}
}