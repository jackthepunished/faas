package api

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// Boot-time env overrides for the operator-tunable subset of the
// pkg/api/limits.go table (issue #899 finding 3).
//
// Almost every limit in limits.go is a compile-time constant on
// purpose: a plan quota that an operator can move is a billing bug
// waiting to happen. The exceptions are the *operational* knobs —
// values that shape how the platform behaves during an incident, not
// what a customer is entitled to. Those get an env override so an
// operator can retune without a redeploy, and they live here rather
// than inline at the call site so limits.go stays the single index of
// "what is tunable and what is not" (CLAUDE.md: never inline a
// limit).
//
// Contract for every knob in this file:
//   - The limits.go constant remains the default and the documented
//     value; the env var only overrides it.
//   - Parsing happens once, at first read, not per request — these
//     are consulted on the gateway hot path.
//   - A malformed or out-of-range value NEVER silently becomes
//     something else: the default is used and the error is retained
//     so the daemon can log it loudly at boot.

// EnvEdgeRuleMaintenanceRetryAfterSeconds is the env var that
// overrides EdgeRuleMaintenanceRetryAfterSeconds. Documented in
// pkg/api/limits.go, migrations/00237_apps_maintenance_mode.sql and
// pkg/gateway/handler.go::applyAppsMaintenanceMode.
const EnvEdgeRuleMaintenanceRetryAfterSeconds = "FAAS_EDGE_RULE_MAINTENANCE_RETRY_AFTER_SECONDS"

// Operator docs pointer stamped into the parse errors below, per the
// CLAUDE.md convention that limit errors carry a docs URL.
const maintenanceRetryAfterDocsURL = "https://docs.gregale.dev/operations/maintenance-mode#retry-after"

// ParseEdgeRuleMaintenanceRetryAfterSeconds parses a raw env value
// into a Retry-After seconds value. An empty string means "unset" and
// yields the platform default. The accepted range is
// [1, MaxEdgeRuleMaintenanceRetryAfterSeconds]: 0 is rejected rather
// than clamped because `Retry-After: 0` is forbidden by RFC 7231 and
// an operator typing 0 means something the platform cannot honour.
//
// Pure and side-effect free so the table test can exercise it without
// touching the process environment.
func ParseEdgeRuleMaintenanceRetryAfterSeconds(raw string) (int, error) {
	if raw == "" {
		return EdgeRuleMaintenanceRetryAfterSeconds, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return EdgeRuleMaintenanceRetryAfterSeconds, fmt.Errorf(
			"%s=%q: not an integer (want 1..%d seconds); see %s",
			EnvEdgeRuleMaintenanceRetryAfterSeconds, raw,
			MaxEdgeRuleMaintenanceRetryAfterSeconds, maintenanceRetryAfterDocsURL)
	}
	if n < 1 || n > MaxEdgeRuleMaintenanceRetryAfterSeconds {
		return EdgeRuleMaintenanceRetryAfterSeconds, fmt.Errorf(
			"%s=%d: out of range (want 1..%d seconds); see %s",
			EnvEdgeRuleMaintenanceRetryAfterSeconds, n,
			MaxEdgeRuleMaintenanceRetryAfterSeconds, maintenanceRetryAfterDocsURL)
	}
	return n, nil
}

var (
	maintenanceRetryAfterOnce  sync.Once
	maintenanceRetryAfterValue int
	maintenanceRetryAfterErr   error
)

func loadMaintenanceRetryAfter() {
	maintenanceRetryAfterValue, maintenanceRetryAfterErr =
		ParseEdgeRuleMaintenanceRetryAfterSeconds(os.Getenv(EnvEdgeRuleMaintenanceRetryAfterSeconds))
}

// EdgeRuleMaintenanceRetryAfter returns the effective platform
// default Retry-After (seconds) for both maintenance gates: the
// coarse apps.maintenance_mode column and a kind=maintenance edge
// rule that carries no per-rule value. It is
// EdgeRuleMaintenanceRetryAfterSeconds (60) unless
// FAAS_EDGE_RULE_MAINTENANCE_RETRY_AFTER_SECONDS overrides it.
//
// A bad env value degrades to the constant — an operator typo must
// not take the maintenance gate out of service — and is reported by
// EdgeRuleMaintenanceRetryAfterEnvErr, which daemons log at boot.
//
// The env is read exactly once, so this is safe on the gateway hot
// path.
func EdgeRuleMaintenanceRetryAfter() int {
	maintenanceRetryAfterOnce.Do(loadMaintenanceRetryAfter)
	return maintenanceRetryAfterValue
}

// EdgeRuleMaintenanceRetryAfterEnvErr returns the error from parsing
// FAAS_EDGE_RULE_MAINTENANCE_RETRY_AFTER_SECONDS, or nil when the env
// var is unset or valid. Daemons call this at boot and log a warning:
// the fallback is safe, but a silently-ignored override is exactly
// the kind of config drift an operator discovers mid-incident.
func EdgeRuleMaintenanceRetryAfterEnvErr() error {
	maintenanceRetryAfterOnce.Do(loadMaintenanceRetryAfter)
	return maintenanceRetryAfterErr
}
