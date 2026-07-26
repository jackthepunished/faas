// envhsts.go — tiny package-level helper to parse FAAS_HSTS_ENABLED.
// Shared between cmd/apid and cmd/gatewayd so the daemon-local
// copies don't trip golangci-lint's goconst over the `"true"` /
// `"false"` literals. Returning bool (not *bool) keeps the call
// sites simple: `httpsec.SetHSTSEnabled(httpsec.HSTSEnabledFromEnv(getenv))`.
//
// The set of truthy tokens is exactly the inverse of the falsy set;
// empty string defaults to true (issue #249). Anything else falls
// through to true as the safe default — a typo in production should
// err on the side of stronger headers, not weaker.
package httpsec

import "strings"

// HSTSEnabledFromEnv reads FAAS_HSTS_ENABLED via the supplied getenv
// (so tests can inject without mutating process env). Default true.
// Truthy tokens: "", "1", "true", "yes", "on" (case-insensitive).
// Falsy tokens: "0", "false", "no", "off" (case-insensitive).
// Anything else → true (safe default).
func HSTSEnabledFromEnv(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("FAAS_HSTS_ENABLED"))) {
	case envHSTSTrue, envHSTSYes, envHSTSOn, envHSTSEmpty, envHSTSOne:
		return true
	case envHSTSFalse, envHSTSNo, envHSTSOff, envHSTSZero:
		return false
	default:
		return true
	}
}

// The string constants are package-private so a future "off means off
// in prod" tweak only has to flip the switch literal — the truthy /
// falsy membership stays self-documenting at the call site.
const (
	envHSTSEmpty = ""     // value: ""
	envHSTSOne   = "1"    // value: "1"
	envHSTSTrue  = "true" // value: "true"
	envHSTSYes   = "yes"  // value: "yes"
	envHSTSOn    = "on"   // value: "on"

	envHSTSZero  = "0"     // value: "0"
	envHSTSFalse = "false" // value: "false"
	envHSTSNo    = "no"    // value: "no"
	envHSTSOff   = "off"   // value: "off"
)
