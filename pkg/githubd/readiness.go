// Package githubd — readiness.go constructs the githubd-side
// /readyz probe (issue #571 PR-A2). Three signals:
//
//   - PG ping: webhook-secret resolver and binding store both
//     round-trip the pool on every push. A degraded pgxpool
//     stalls every inbound webhook; /readyz flips to 503 so
//     the loopback LB stops routing.
//   - GitHub App credentials loaded: realSvc != nil signals
//     that OAuth + Checks are live. A dev box without App
//     credentials stays up for the webhook path (fail-closed
//     but stay-up per the slice 8 contract); /readyz reflects
//     the degraded state so the operator dashboard surfaces it.
//   - Webhook secret resolver wired: SecretResolver != nil.
//     Production always wires NewPGWebhookSecretResolver at
//     boot; nil means the per-tenant secret path fell back
//     to the platform-wide env, which is a deploy misconfig
//     we want /readyz to catch.
//
// The check functions are exposed as a BuildReadinessProbe
// helper that returns a wire.ReadyzProbe + stop func. The
// Server struct exposes ReadyFunc + ReasonFunc fields so
// cmd/githubd/main.go can wire the probe after construction.
package githubd

import (
	"context"
	"time"

	"github.com/onebox-faas/faas/pkg/wire"
)

// pgPool is the subset of *pgxpool.Pool we need for the
// PG ping signal. Same shape as cmd/schedd/readiness.go::pgPool
// and cmd/builderd/readiness.go::pgPool — kept local so the
// test path doesn't pull pgxpool into the binary.
type pgPool interface {
	Ping(ctx context.Context) error
}

// appCredsLoaded reports whether the GitHub App credentials
// (AppID + private key + client id/secret) were provisioned
// at boot. The signal is wired in cmd/githubd/main.go via a
// closure over realSvc != nil after the OAuth branch decides
// whether to construct RealService.
type appCredsLoaded func() bool

// secretResolverWired reports whether the per-tenant webhook
// secret resolver was wired at boot. Production always wires
// NewPGWebhookSecretResolver; nil means the per-tenant secret
// path fell back to the platform-wide env.
type secretResolverWired func() bool

// BuildReadinessProbe constructs the githubd /readyz probe.
// pool is the *pgxpool.Pool used by the binding store; nil
// degrades the probe to a single "pg pool nil (test path)"
// signal. credsLoaded + secretWired are closure hooks the
// boot path uses to gate the other two signals.
func BuildReadinessProbe(ctx context.Context, pool pgPool, credsLoaded appCredsLoaded, secretWired secretResolverWired) *wire.ReadyzProbe {
	p := &wire.ReadyzProbe{}
	if pool != nil {
		sig, stop := wire.NewPGPingSignal(ctx, pool, 5*time.Second)
		p.RegisterSignal(sig, stop)
	} else {
		s := p.Register()
		s.Set(false, "pg pool nil (test path)")
	}
	if credsLoaded != nil {
		p.RegisterSignal(credsLoadedSignal(credsLoaded, 5*time.Second), nil)
	}
	if secretWired != nil {
		p.RegisterSignal(secretWiredSignal(secretWired, 5*time.Second), nil)
	}
	return p
}

// credsLoadedSignal polls credsLoaded on a 5 s cadence. The
// call is cheap (a struct nil check) so we don't cache the
// result — every probe tick is fine.
func credsLoadedSignal(credsLoaded appCredsLoaded, _ time.Duration) *wire.ReadySignal {
	s := &wire.ReadySignal{}
	s.Set(credsLoaded(), "")
	return s
}

// secretWiredSignal polls secretWired on a 5 s cadence. Same
// rationale as credsLoadedSignal — the check is a nil
// comparison, not a syscall.
func secretWiredSignal(secretWired secretResolverWired, _ time.Duration) *wire.ReadySignal {
	s := &wire.ReadySignal{}
	s.Set(secretWired(), "")
	return s
}
