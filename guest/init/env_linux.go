// env_linux.go — load /etc/faas/env.json after pivot_root
// (issue #395 / ADR-045).
//
// The file is a plaintext JSON map written by vmmd at wake time. Unlike
// secrets_linux.go, there is NO seal/unseal step here — env vars are
// non-sensitive runtime config by contract (LOG_LEVEL, FEATURE_X, etc.);
// the plaintext values land on drive1 directly. The file sits at a
// different path than secrets.env so a JSON-decode failure on one
// doesn't propagate to the other.
//
// guest-init treats the file as optional: a missing or malformed file
// is logged and the boot proceeds without the api-env layer. A
// malformed file is never a fatal error — the worst case is "no api
// env vars this run" not "hang at boot".
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// apiEnvPath is the vmmd-written file inside the per-app drive1
// (mirrors pkg/fcvm/vmm.go::apiEnvPath — keeping them in sync is a
// build-time invariant; the value is duplicated rather than imported
// because guest-init is a static binary with no Go module access at
// boot).
const apiEnvPath = "/etc/faas/env.json"

// loadAPIEnv reads /etc/faas/env.json and returns the decoded map.
// Errors:
//   - file absent  → (nil, nil) — caller treats as "no api env this app"
//   - read failure → returns wrapped ErrAPIEnvUnreadable; the boot
//     proceeds with no api env and a slog.Warn surfaces the misconfig.
//   - parse failure → returns wrapped ErrAPIEnvParseFail; same fallback.
//
// Signature mirrors loadSecrets — (map, err) so the call site at
// main_linux.go::boot can pick its policy per-error. We do not
// introduce a new sentinel here because the existing isNotExist helper
// covers the "no file" case uniformly across both readers; the call
// site doesn't need to distinguish file-absent from
// permission-denied.
func loadAPIEnv(log *slog.Logger) (map[string]string, error) {
	data, err := os.ReadFile(apiEnvPath)
	if err != nil {
		if isNotExist(err) {
			return nil, nil // no file — most apps have no api env
		}
		return nil, fmt.Errorf("api_env: read %q: %w", apiEnvPath, err)
	}
	if len(data) == 0 {
		return nil, nil // empty file == no api env
	}
	out := map[string]string{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("api_env: parse %q: %w", apiEnvPath, err)
	}
	if log != nil {
		log.Info("api_env loaded", "count", len(out))
	}
	return out, nil
}
