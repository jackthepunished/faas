// session_key.go — load the AEAD session-manager key from
// FAAS_SESSION_KEY (Move 4 PR-2). The app-logs route walks through
// pkg/auth.Middleware.RequireSession, whose session-cookie branch
// AEAD-verifies the cookie envelope — that requires a per-daemon
// *session.Manager. The key material lives in /etc/faas/secrets/
// session.key as a hex-encoded 32-byte string; the env wrapper
// keeps the secrets dir unchanged.
//
// We deliberately do NOT lift the cmd/apid loader:
// cmd/apid/loadSessionManager is package-private to cmd/apid and
// inlined into the apid boot path. Cd/gatewayd duplicates the env
// parsing (8 lines) so the two daemons stay independent — the
// AEAD keys are per-process, and a shared helper would imply a
// shared key path, which crosses the per-daemon secret boundary
// (spec §11).
//
// The cmd/apid loader (cmd/apid/handlers_auth.go::loadSessionManager)
// supports BOTH the PATH-shaped env-var contract (systemd
// LoadCredential + Environment=KEY=%d/<id> → os.ReadFile on the path)
// and the CONTENT-shaped contract (raw hex in the env). gatewayd-internal
// delivers FAAS_SESSION_KEY via per-daemon EnvironmentFile= (issue
// #585 / ADR-127) so CONTENT-shaped is the canonical contract here;
// the PATH-shaped branch is a defense-in-depth mirror of the apid
// loader's behaviour so a misconfigured systemd unit doesn't silently
// fall through to ephemeral mode (the A5 silent-degradation bug
// closed by PR #1075 review-fix R1+R2).
package main

import (
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/session"
)

// loadSessionManager matches cmd/apid/handlers_auth.go::loadSessionManager
// for the env contract — both daemons read FAAS_SESSION_KEY the same
// way so a misconfigured unit gets caught loud instead of silently
// falling back to ephemeral mode. Empty env value → ephemeral manager
// + warning (dev fallback). PATH-shaped input (leading "/" + existing
// regular file) is read via os.ReadFile; CONTENT-shaped input (raw
// hex) is decoded in place.
func loadSessionManager(getenv func(string) string, log *slog.Logger) *session.Manager {
	raw := strings.TrimSpace(getenv("FAAS_SESSION_KEY"))
	if raw == "" {
		m, err := session.NewEphemeralManager(7 * 24 * time.Hour)
		if err != nil {
			log.Error("gatewayd: ephemeral session manager failed", "err", err)
			return nil
		}
		log.Warn("FAAS_SESSION_KEY unset; ephemeral session key in use (dev only)")
		return m
	}
	// PATH-shaped branch — mirrors cmd/apid/handlers_auth.go so a
	// future migration of gatewayd-internal to LoadCredential stays
	// a one-line unit change without an apid-side ripple.
	if strings.HasPrefix(raw, "/") {
		if info, err := os.Stat(raw); err == nil && info.Mode().IsRegular() {
			data, readErr := os.ReadFile(raw)
			if readErr != nil {
				log.Error("FAAS_SESSION_KEY path read failed",
					"path", raw, "err", readErr)
				return nil
			}
			raw = strings.TrimSpace(string(data))
			log.Info("FAAS_SESSION_KEY loaded via LoadCredential path",
				"path", raw, "mode", info.Mode().String())
		}
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		// Not hex (or odd length). Distinct from a wrong-byte-length
		// failure so the operator can tell from the log line which
		// axis is broken — the v1 bootstrap.sh script emitted the
		// canonical 64-hex string (RETIRED 2026-08-15 by issue #911 /
		// PR-1; v2 path is PR-X `gregale secrets init`), but a
		// hand-edited secrets file could easily truncate or paste
		// non-hex bytes.
		log.Error("FAAS_SESSION_KEY is not valid hex", "got_len", len(raw), "err", err)
		return nil
	}
	if len(key) != 32 {
		log.Error("FAAS_SESSION_KEY has wrong byte length", "got_bytes", len(key), "want_bytes", 32)
		return nil
	}
	m, err := session.NewManager(key, 7*24*time.Hour)
	if err != nil {
		log.Error("gatewayd: session manager build failed", "err", err)
		return nil
	}
	return m
}
