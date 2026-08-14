// pkg/api/flags.go — env-driven feature flags that gate customer
// surface wiring. The TenantSurfaces flag is the dark-launch switch
// for issue #879 / ADR-100: the schema and the apid routes are
// wired in PR-C, but the routes 404 (or 503) until this flag is
// set, so a misconfigured rollout can be reverted by simply
// unsetting the env var (no migration to undo, no DNS to withdraw).
//
// Pattern mirrors cmd/apid/server.go:189-203 for FAAS_REKEY_ENABLED
// — direct os.Getenv with a stable "1" / "true" / "yes" accept
// set, and a default-off shape. No global mutable state outside
// the accessor function so tests can override with t.Setenv.
package api

import (
	"os"
	"strings"
)

// TenantSurfacesEnabled reports whether the customer surface
// HTTP API is live. Reads FAAS_TENANT_SURFACES_ENABLED at every
// call (not cached at boot) so an operator can flip the env var
// and SIGHUP-restart-free roll out / roll back the surface routes
// without bouncing every daemon. Default off; the cert engine +
// state surface are in place but the HTTP routes + CLI are
// gated until PR-C ships.
func TenantSurfacesEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FAAS_TENANT_SURFACES_ENABLED")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
