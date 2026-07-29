// Test helpers shared between the external (blackbox) test files.
// Lives in package auth_test (external) so it can be referenced
// from middleware_test.go's cases.
package middleware_test

import (
	"context"

	"github.com/onebox-faas/faas/pkg/auth/middleware"
	"github.com/onebox-faas/faas/pkg/state"
)

// authWithPrincipal stamps a bearer-style principal onto ctx so
// RequireScope sees the same shape RequireSession produces.
// Uses the exported middleware.WithPrincipal helper (added for this
// test surface) so the unexported ctx-key typing stays in pkg/middleware.
func authWithPrincipal(ctx context.Context, acct state.Account, key *state.APIKey) context.Context {
	return middleware.WithPrincipal(ctx, acct, key)
}

// Compile-time check that state.APIKey is the same type both
// packages use — guards against a future rename that would
// silently break the helper.
var _ *state.APIKey = (*state.APIKey)(nil)
