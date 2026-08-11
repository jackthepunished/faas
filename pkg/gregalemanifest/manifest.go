// Package gregalemanifest — loader for the `gregale.yaml` /
// `gregale.yml` declarative manifest (issue #791 PR-C / ADR-090).
//
// Scope (PR-C): only `triggers:` is recognised. Future kinds
// (`event|queue`) slot in under the same `Kind` discriminator without
// a schema bump — see ADR-090 §"triggers: manifest key".
//
// File discovery: the loader takes a project dir and looks for
// `gregale.yaml` first, then `gregale.yml`. A TOML file
// (`gregale.toml`) is rejected with an explicit error per ADR-090
// §"YAML vs TOML" — silent ignoring would let customers think their
// manifest was applied when it wasn't.
//
// Why a shared package, not `cmd/gregale/manifest.go`: the long-term
// plan (per the plan's "loader location" section) is to also validate
// the same schema server-side in `cmd/apid/scan_service.go`. A shared
// package avoids a cmd→cmd import and keeps the parser's failure
// modes (UnknownKind, BadSchedule, PathNoSlash, Duplicate) in one
// place that both surfaces can reuse.
package gregalemanifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/onebox-faas/faas/pkg/sched"
)

// TriggerKind is the closed vocabulary for `triggers[].kind`. PR-C
// ships only `cron`; future kinds slot in without a schema bump.
type TriggerKind string

const (
	TriggerKindCron TriggerKind = "cron"
)

// Trigger is one entry under `triggers:`. Mirrors `api.PlanCron` plus
// the `kind` discriminator. `Enabled` is a pointer so the YAML
// decoder can distinguish "absent" from "explicit false" — the spec
// is "absent → true" (a trigger with no `enabled:` line is enabled).
type Trigger struct {
	Kind     TriggerKind `yaml:"kind"`
	App      string      `yaml:"app"`
	Schedule string      `yaml:"schedule"`
	Path     string      `yaml:"path"`
	Enabled  *bool       `yaml:"enabled,omitempty"`
}

// IsEnabled returns the trigger's effective enabled state. nil pointer
// (key absent in the YAML) defaults to true — opt-out semantics match
// the `CreateCron` API where omitted `enabled` defaults to true.
func (t Trigger) IsEnabled() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// Manifest is the parsed `gregale.yaml` root. Only `triggers` is
// recognised in PR-C; other top-level keys are validated strictly
// (yaml.Decoder.KnownFields(true)) so a typo like `trigger:` (singular)
// surfaces as a load-time error rather than silently shipping a
// no-op deploy.
type Manifest struct {
	Triggers []Trigger `yaml:"triggers"`
}

// Load reads `gregale.yaml` or `gregale.yml` from dir. Returns
// (nil, false, nil) when no manifest is present — callers treat this
// as "no work to do" without special-casing the error. On parse
// failure returns a wrapped error with the file path so a
// `gregale deploy` invocation reports `gregale.yaml: ...`.
func Load(dir string) (*Manifest, bool, error) {
	for _, name := range []string{"gregale.yaml", "gregale.yml"} {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, fmt.Errorf("gregalemanifest: read %s: %w", path, err)
		}
		m, err := parseManifest(b)
		if err != nil {
			return nil, false, fmt.Errorf("gregalemanifest: parse %s: %w", path, err)
		}
		return m, true, nil
	}
	// Explicit rejection: a TOML manifest is left untouched by Load
	// (caller sees no-op) but the presence of `gregale.toml` is a
	// hard error. This catches the "I wrote toml but Load silently
	// ignored it" footgun.
	if _, err := os.Stat(filepath.Join(dir, "gregale.toml")); err == nil {
		return nil, false, errors.New("gregalemanifest: gregale.toml is present but TOML manifests are not supported yet (rename to gregale.yaml)")
	}
	return nil, false, nil
}

// parseManifest decodes the bytes with strict unknown-field rejection.
// Without KnownFields(true), a typo'd `trigger:` (singular) would
// silently drop every entry — the customer's deploy would ship a
// no-op `triggers:` and they'd discover the gap in production. Strict
// decoding turns the typo into a load-time error.
func parseManifest(b []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	m := &Manifest{}
	if err := dec.Decode(m); err != nil {
		// yaml.Decoder wraps a strict-decode failure as a
		// *yaml.TypeError; we surface the inner message verbatim.
		return nil, fmt.Errorf("decode: %w", err)
	}
	return m, nil
}

// Validate runs schema checks against the decoded manifest. Triggered
// before any `CreateCron` fan-out in `cmdDeployTarball` so a typo'd
// schedule aborts the deploy before any cron row is mutated.
//
// Validation order matches the failure modes a customer would debug
// most often: kind first (so an unknown kind surfaces as a clear
// upgrade-me message), then schedule (the most likely typo), then
// path + app + duplicates. The duplicate check is last because it's
// the most expensive and only meaningful when the per-entry checks
// pass.
func (m *Manifest) Validate() error {
	if m == nil {
		return nil
	}
	seen := make(map[triggerKey]struct{}, len(m.Triggers))
	for i, t := range m.Triggers {
		switch t.Kind {
		case TriggerKindCron:
			// fall through
		case "":
			return fmt.Errorf("trigger[%d]: missing kind (want %q)", i, TriggerKindCron)
		default:
			return fmt.Errorf("trigger[%d]: unsupported trigger kind %q (only %q is supported in this release)",
				i, t.Kind, TriggerKindCron)
		}
		if _, err := sched.ParseSchedule(t.Schedule); err != nil {
			return fmt.Errorf("trigger[%d]: bad schedule %q: %w", i, t.Schedule, err)
		}
		if !strings.HasPrefix(t.Path, "/") {
			return fmt.Errorf("trigger[%d]: path must start with '/' (got %q)", i, t.Path)
		}
		if t.App == "" {
			return fmt.Errorf("trigger[%d]: app is required", i)
		}
		k := triggerKey{app: t.App, schedule: t.Schedule, path: t.Path}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("trigger[%d]: duplicate (app, schedule, path) triple — %q / %q / %q",
				i, t.App, t.Schedule, t.Path)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// triggerKey is the dedupe primitive. Three fields because two crons
// with the same app + schedule but different paths are different
// resources — the (app, schedule, path) tuple is enforced by the
// crons_app_schedule_path_unique constraint added in
// migrations/00207 (issue #791 PR-E / ADR-090 closure).
type triggerKey struct {
	app      string
	schedule string
	path     string
}
