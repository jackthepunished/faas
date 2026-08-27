// commands_obs.go — Obs-Meta + Trace-IDs Mega-PR / C8:
// operator-side `gregalectl obs` subcommand dispatcher.
//
// Subcommands:
//
//	gregalectl obs health    — fetch GET /v1/admin/obs/health
//	                           from the local apid and emit
//	                           either a human-readable summary
//	                           (default) or the raw JSON snapshot
//	                           (--json / $FAAS_JSON=1).
//
// The CLI dials apid via HTTP (not gRPC) — same path the operator
// would use from a browser. The endpoint requires admin scope +
// MFA + FAAS_ADMIN_EMAILS allowlist, so the CLI needs the same
// admin bearer that the existing /v1/admin/* surface uses.
//
// apid URL resolution: $FAAS_APID_URL wins; defaults to
// http://127.0.0.1:8080 (the apid loopback listen addr on
// control-plane nodes; matches the convention at
// cmd/apid/main.go::resolveListenAddr).
//
// Out of scope (explicit, mirrors the C7 plan): `obs events`,
// `obs incidents`, sub-subcommands. The dispatcher reserves
// room for those; this PR lands only the meta-obs health
// surface the C7 endpoint exposes.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// dispatchObs is the top-level `obs` command name. Matches the
// cli_meta.go entry below + the main.go case dispatchObs arm.
// Used as both the dispatch key AND the cliCommand.Name so the
// manifest-drift test in commands_completion_test.go catches
// any future rename.
const dispatchObs = "obs"

// subObsHealth is the `obs health` subcommand name. Exported
// indirectly via cli_meta.go; keep the constant in this file
// rather than cli_meta.go so the dispatcher's switch is the
// single source of truth for subcommand routing.
const subObsHealth = "health"

// defaultAPIDURL is the loopback default for the apid listen
// addr. Matches cmd/apid/main.go::resolveListenAddr default of
// :8080 on loopback. Operators running apid behind a public
// hostname should set FAAS_APID_URL explicitly.
const defaultAPIDURL = "http://127.0.0.1:8080"

// cmdObsDispatch routes the `gregalectl obs` top-level command
// to its subcommand handlers. Mirrors the shape of
// cmdInstancesDispatch (commands_instances.go:75) and
// cmdBuildsDispatch (commands_builds.go:32): if no subcommand
// is supplied, the dispatcher prints a stable usage line + a
// 2 exit code (per the spec convention at commands_manifest.go
// for `manifest validate` without --file).
func cmdObsDispatch(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "gregalectl obs: missing subcommand; want health")
		return 2
	}
	switch args[0] {
	case subObsHealth:
		return cmdObsHealth(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "gregalectl obs: unknown subcommand %q\nRun 'gregalectl obs health --help' for usage.\n", args[0])
		return 2
	}
}

// apidBase returns the apid base URL, overridable via
// $FAAS_APID_URL for local/dev. Mirrors cmd/gregale/config.go
// ::apiBase — kept as a per-binary helper because the operator
// binary and the customer binary target different surfaces
// (FAAS_APID_URL vs FAAS_API) and the defaults differ.
func apidBase() string {
	if v := os.Getenv("FAAS_APID_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultAPIDURL
}

// cmdObsHealth fetches GET /v1/admin/obs/health and either
// pretty-prints a human summary (default) or emits the raw JSON
// snapshot (--json / $FAAS_JSON=1).
//
// Human summary shape (mirrors the dashboard's red/yellow/green
// tile conventions so an on-call reading the CLI gets the same
// gist without parsing JSON):
//
//	audit_log_write_total_5m:           1234
//	audit_log_write_failures_5m:            0
//	audit_log_coverage_ratio_5m:        1.00
//	operator_intent_outcome_missing_total:
//	  force_park:        0
//	  force_cold_boot:   0
//	  force_restart:     0
//	trace_id_completeness_ratio:
//	  force_park:        1.00
//	  force_cold_boot:   1.00
//	  force_restart:     1.00
//	alerts_firing:                       0
//
// --json / FAAS_JSON=1 overrides the human format. Both paths
// emit stable, greppable output (no ANSI codes; the operator's
// pager-friendly escape sequences are the dashboard's job).
func cmdObsHealth(args []string) int {
	fs := flag.NewFlagSet(subObsHealth, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit structured JSON to stdout (overrides human summary)")
	adminToken := fs.String("admin-token", "", "admin bearer for the FAAS_ADMIN_EMAILS-allowlisted admin scope (default: $FAAS_ADMIN_TOKEN)")
	timeout := fs.Duration("timeout", 10*time.Second, "HTTP timeout for the apid round-trip (default 10s)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Resolve bearer: --admin-token wins, then $FAAS_ADMIN_TOKEN.
	// Empty bearer surfaces a stable error so the operator can
	// tell "no auth supplied" from "apid rejected the request".
	token := *adminToken
	if token == "" {
		token = os.Getenv("FAAS_ADMIN_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "gregalectl obs health: --admin-token (or $FAAS_ADMIN_TOKEN) required (admin scope is gated)")
		return 2
	}

	// Build the GET request. The endpoint is /v1/admin/obs/health;
	// no query params — the snapshot is windowless (the handler
	// hard-codes a 5m window for PromQL + SQL aggregates).
	url := apidBase() + "/v1/admin/obs/health"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl obs health:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: *timeout}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl obs health: dial apid:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl obs health: read body:", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		// Stable error shape so the on-call can grep for the
		// status code + a JSON problem envelope (matches the
		// pattern at cmd/gregale/commands5.go for the customer
		// HTTP surface).
		fmt.Fprintf(os.Stderr, "gregalectl obs health: apid returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return 1
	}

	if *jsonOut || jsonFlagEnabled(args) {
		// Raw JSON pass-through. Pretty-print only when the
		// output is going to a terminal — pipes get the dense
		// shape so downstream tooling can grep.
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			fmt.Fprintln(os.Stderr, "gregalectl obs health: decode response:", err)
			return 1
		}
		enc := json.NewEncoder(os.Stdout)
		if isTerminal(os.Stdout) {
			enc.SetIndent("", "  ")
		}
		if err := enc.Encode(v); err != nil {
			fmt.Fprintln(os.Stderr, "gregalectl obs health: encode json:", err)
			return 1
		}
		return 0
	}

	// Human summary. Decode into a permissive map so unknown
	// fields are tolerated; the closed-set keys are the only
	// ones we render.
	var snap map[string]any
	if err := json.Unmarshal(body, &snap); err != nil {
		fmt.Fprintln(os.Stderr, "gregalectl obs health: decode response:", err)
		return 1
	}
	writeObsHealthHuman(os.Stdout, snap)
	return 0
}

// jsonFlagEnabled reports whether --json or $FAAS_JSON=1 was
// supplied. Mirrors the applyJSONFlag helper at
// commands5.go:94 — duplicated here so the obs subcommand
// stays self-contained rather than importing the customer
// binary's flag-walk. Per the C8 plan, the existing
// gregalectl/json_flag.go convention is honoured; if a future
// PR adds more CLI-side --json dispatchers, hoist this helper
// into the shared internal package.
func jsonFlagEnabled(args []string) bool {
	if os.Getenv("FAAS_JSON") == "1" {
		return true
	}
	for _, a := range args {
		if a == "--json" || a == "--json=1" {
			return true
		}
	}
	return false
}

// isTerminal reports whether w is a terminal (vs a pipe or
// file). Used by cmdObsHealth to decide whether to pretty-print
// the JSON output. Stdlib-only — no termcap dependency.
func isTerminal(w *os.File) bool {
	fi, err := w.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// writeObsHealthHuman renders the closed-set snapshot as a
// flat greppable summary. Subcommand-level helper so the
// handler stays ≤50 lines and the human-shape contract has
// its own focused test file (commands_obs_test.go).
//
// Field order matches api.ObsHealthResponse JSON tags so a
// `jq` consumer can pin the order too.
func writeObsHealthHuman(w io.Writer, snap map[string]any) {
	_, _ = fmt.Fprintln(w, "audit_log_write_total_5m:", snap["audit_log_write_total_5m"])
	_, _ = fmt.Fprintln(w, "audit_log_write_failures_5m:", snap["audit_log_write_failures_5m"])
	_, _ = fmt.Fprintln(w, "audit_log_coverage_ratio_5m:", snap["audit_log_coverage_ratio_5m"])
	_, _ = fmt.Fprintln(w, "operator_intent_outcome_missing_total:")
	writeObsHealthKindBlock(w, snap["operator_intent_outcome_missing_total"], false)
	_, _ = fmt.Fprintln(w, "trace_id_completeness_ratio:")
	writeObsHealthKindBlock(w, snap["trace_id_completeness_ratio"], true)
	_, _ = fmt.Fprintln(w, "alerts_firing:", snap["alerts_firing"])
}

// writeObsHealthKindBlock renders a per-kind sub-map
// (operator_intent_outcome_missing_total or
// trace_id_completeness_ratio) as a stable alphabetical
// list. pretty=true is reserved for future use (rational
// formatting); today both modes render the raw value.
func writeObsHealthKindBlock(w io.Writer, raw any, pretty bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		_, _ = fmt.Fprintln(w, "  (absent)")
		return
	}
	// Stable alphabetical iteration: pull the keys, sort them,
	// render. Matches the dashboard's tile-mapping convention.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for _, k := range sortedObsHealthKeys(keys) {
		v := m[k]
		if pretty {
			switch x := v.(type) {
			case float64:
				_, _ = fmt.Fprintf(w, "  %s: %.2f\n", k, x)
				continue
			}
		}
		_, _ = fmt.Fprintf(w, "  %s: %v\n", k, v)
	}
}

// sortedObsHealthKeys returns the keys in lexical order so the
// CLI's output is stable across runs (map iteration order in
// Go is randomized). Extracted so commands_obs_test.go can
// pin the order without copy-pasting the sort.
func sortedObsHealthKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	// insertion sort — keys are short (≤ 8 operator-action
	// kinds); insertion sort beats sort.Strings for n < 24.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
