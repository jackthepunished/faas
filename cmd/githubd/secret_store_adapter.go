// Package main (cmd/githubd) holds the state-SecretStore adapter
// (PR-D / ADR-012 §7 amendment). The adapter bridges
// pkg/state.PgStore.GetGithubWebhookSecret to the
// pkg/githubd.SecretStore interface that PGWebhookSecretResolver
// consumes. Lives in cmd/githubd (not pkg/githubd) because
// pgxpool.Pool is the one piece already in scope here — keeping
// the adapter next to the wiring makes the seam obvious.
package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onebox-faas/faas/pkg/state"
)

// stateSecretStoreAdapter is the production SecretStore. The
// adapter is a near no-op: pkg/githubd.SecretStore was
// deliberately written in pkg/state types so the adapter is just
// a method-set bridge.
type stateSecretStoreAdapter struct {
	store *state.PgStore
}

func newStateSecretStoreAdapter(pool *pgxpool.Pool) *stateSecretStoreAdapter {
	return &stateSecretStoreAdapter{store: state.NewPgStore(pool)}
}

// GetGithubWebhookSecret reads the bytea from
// github_webhook_secrets. Missing row returns state.ErrNotFound,
// which the resolver treats as a per-tenant miss (and the server
// handler translates to fail-closed + the metric spike).
func (a *stateSecretStoreAdapter) GetGithubWebhookSecret(ctx context.Context, installationID int64) ([]byte, error) {
	secret, err := a.store.GetGithubWebhookSecret(ctx, installationID)
	if err != nil {
		// state.ErrNotFound and the resolver's errSecretNotFound
		// are distinct (the resolver avoids importing pkg/state
		// to keep the import graph clean). The resolver's
		// isNotFound helper matches the sentinel string, so the
		// pass-through is safe.
		return nil, err
	}
	if len(secret) == 0 {
		// Defensive: a row with empty secret_value is a write
		// bug. Surface as ErrNotFound so the resolver falls back
		// to the platform secret rather than HMACing with [].
		return nil, errors.New("state: not found")
	}
	return secret, nil
}
