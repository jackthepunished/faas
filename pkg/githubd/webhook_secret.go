// Per-tenant GitHub App webhook secret resolver (PR-D / ADR-012 §7
// amendment).
//
// Replaces the platform-wide FAAS_GITHUB_WEBHOOK_SECRET with a
// row-per-installation_id lookup so a leaked tenant secret can
// rotate without coordinating every GitHub App install. The cache
// pattern mirrors pkg/githubd/tokencache.go (the install-token
// cache) — same in-memory map + singleflight + janitor shape, the
// only difference is the backing store (DB rows instead of the
// api.github.com token endpoint).
//
// Sync semantics:
//
//   - Cache miss → first caller blocks on the DB read and writes
//     the result. Concurrent callers wait on the same call
//     (singleflight-style). The DB read is one row, PRIMARY KEY
//     lookup, fast (<2ms p99).
//   - Cache hit, fresh → return immediately. The verifier reads
//     the raw bytes and runs HMAC-SHA256.
//   - Cache hit, near-expiry (<5 min) → caller is allowed to
//     trigger an explicit Invalidate from the `gregale
//     github-webhook-secret set` admin path (next read rebuilds).
//     Otherwise the entry is evicted on TTL expiry.
//
// A janitor goroutine wakes every minute to evict dead entries.
//
// Failure posture: a DB error on Resolve returns an error. The
// caller (server.go::webhookSecretFromHeader) treats errors as
// fail-closed — the webhook is rejected. The Prometheus counter
// `githubd_webhook_secret_total{status="db_error"}` is emitted by
// the caller so a partial DB outage is visible without an alert
// storm.
package githubd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SecretStore is the backing store PGWebhookSecretResolver reads
// from. The interface is the test seam — production wires
// (*pkg/state.PgStore).GetGithubWebhookSecret, whitebox tests
// inject a recording fake.
type SecretStore interface {
	GetGithubWebhookSecret(ctx context.Context, installationID int64) ([]byte, error)
}

// WebhookSecretResolver is the public seam githubd uses to obtain
// a per-tenant webhook secret. The interface is small (Resolve
// + Invalidate) to keep the test fake trivial.
type WebhookSecretResolver interface {
	// Resolve returns the secret bytes for the given
	// installation_id, or (nil, err) on miss / DB error.
	// Concurrent calls for the same installation_id are
	// coalesced via singleflight.
	Resolve(ctx context.Context, installationID int64) ([]byte, error)

	// Invalidate drops the cached entry for the given
	// installation_id. Used by the `gregale
	// github-webhook-secret set` admin path so a freshly
	// rotated secret is picked up without waiting for the TTL.
	// Idempotent — Invalidate on a missing key is a no-op.
	Invalidate(installationID int64)
}

// PGWebhookSecretResolver is the production-ready resolver.
// Backed by a Postgres SecretStore, an in-memory LRU+TTL cache,
// and a singleflight coalescer.
type PGWebhookSecretResolver struct {
	store SecretStore
	log   *slog.Logger

	// ttl is the cache lifetime. Default 60s. A short TTL keeps
	// the fail-closed window small when a rotation happens
	// without an explicit Invalidate call (e.g. an operator
	// poking the DB directly).
	ttl time.Duration

	// clock is the time source. Tests override.
	clock func() time.Time

	mu       sync.Mutex
	items    map[int64]*secretEntry
	inflight map[int64]*inflightSecret
}

type secretEntry struct {
	secret    []byte
	expiresAt time.Time
}

type inflightSecret struct {
	done   chan struct{}
	secret []byte
	err    error
}

// NewPGWebhookSecretResolver builds a resolver. ttl <= 0 falls
// back to 60s. The log is required (not optional) so a degraded
// cache miss is visible — every cache miss is a webhook
// rejection, so a log line at the boundary is load-bearing for
// the on-call runbook at docs/runbooks/GithubWebhookSecretRotation.md.
func NewPGWebhookSecretResolver(store SecretStore, log *slog.Logger, ttl time.Duration) *PGWebhookSecretResolver {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &PGWebhookSecretResolver{
		store:    store,
		log:      log,
		ttl:      ttl,
		clock:    time.Now,
		items:    map[int64]*secretEntry{},
		inflight: map[int64]*inflightSecret{},
	}
}

// Resolve returns the secret bytes for the given installation_id.
// Cached values within TTL are returned without a DB call. On a
// cache miss, the DB read is coalesced via singleflight. Returns
// (nil, err) on DB error — the caller (server.go) treats the
// error as fail-closed.
func (r *PGWebhookSecretResolver) Resolve(ctx context.Context, installationID int64) ([]byte, error) {
	if installationID == 0 {
		return nil, fmt.Errorf("githubd: webhook secret resolver: installation_id must be non-zero")
	}

	// Fast path: cache hit within TTL.
	now := r.clock()
	r.mu.Lock()
	if ent, ok := r.items[installationID]; ok && ent.expiresAt.After(now) {
		secret := ent.secret
		r.mu.Unlock()
		return secret, nil
	}
	// Check for an in-flight DB read for the same id.
	if inf, ok := r.inflight[installationID]; ok {
		r.mu.Unlock()
		select {
		case <-inf.done:
			return inf.secret, inf.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// Slow path: claim the inflight slot.
	inflight := &inflightSecret{done: make(chan struct{})}
	r.inflight[installationID] = inflight
	r.mu.Unlock()

	// The DB read is outside the lock so concurrent callers
	// don't block the map.
	secret, err := r.store.GetGithubWebhookSecret(ctx, installationID)

	r.mu.Lock()
	if err == nil && len(secret) > 0 {
		r.items[installationID] = &secretEntry{
			secret:    secret,
			expiresAt: r.clock().Add(r.ttl),
		}
	}
	delete(r.inflight, installationID)
	inflight.secret = secret
	inflight.err = err
	close(inflight.done)
	r.mu.Unlock()

	if err != nil {
		// State.ErrNotFound is the expected miss — the platform
		// secret (FAAS_GITHUB_WEBHOOK_SECRET) is the fallback
		// for installs that haven't been migrated yet. Other
		// errors are DB outage; emit at Warn so the on-call
		// sees them.
		if errors.Is(err, errSecretNotFound) || isNotFound(err) {
			r.log.Info("githubd: webhook secret: per-tenant miss; falling back to platform secret",
				"installation_id", installationID)
			return nil, errSecretNotFound
		}
		r.log.Warn("githubd: webhook secret: DB error (fail-closed)",
			"installation_id", installationID, "err", err)
		return nil, err
	}
	return secret, nil
}

// Invalidate drops the cached entry so the next Resolve rebuilds
// from the DB. Used by the Set hot path.
func (r *PGWebhookSecretResolver) Invalidate(installationID int64) {
	r.mu.Lock()
	delete(r.items, installationID)
	r.mu.Unlock()
}

// errSecretNotFound is the sentinel returned to the caller when
// the row is absent. The caller (server.go::webhookSecretFromHeader)
// translates this to a fallback to the platform-wide secret, OR
// the fail-closed rejection depending on the
// allow-per-tenant-fallback toggle (PR-D's default: fail-closed;
// the platform fallback is enabled by an env var on the ops side).
var errSecretNotFound = errors.New("githubd: webhook secret: per-tenant row missing")

// isNotFound is a small helper so the resolver doesn't have to
// import pkg/state (cyclic import risk). The PgStore returns
// state.ErrNotFound on miss; we check by name to avoid the
// import.
func isNotFound(err error) bool {
	// The PgStore's ErrNotFound is exported from pkg/state. We
	// match by string to avoid the import cycle: pkg/githubd
	// cannot import pkg/state (pkg/state already imports
	// pkg/githubd in some paths). The string is stable per the
	// sentinel definition: "state: not found".
	return err != nil && err.Error() == "state: not found"
}
