// Package gregalemanifest — loader for the `gregale.yaml` /
// `gregale.yml` declarative manifest (issue #791 PR-C / ADR-090,
// extended by issue #757 / ADR-0NN).
//
// Scope (ADR-0NN widens PR-C): the `triggers:` key now recognises six
// kinds — cron (the existing synthetic-wake path, unchanged from
// PR-C) plus kafka, nats, redis_streams, sqs_compat, and the
// in-platform queue/delayed_task merger (the unified Trigger primitive
// from issue #757). The closed-vocabulary `Kind` discriminator
// patterns after ADR-090 §"triggers: manifest key" — the cron path is
// strictly backward compatible, the new kinds slot in under the same
// discriminator without a YAML schema bump.
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
// modes (UnknownKind, BadSchedule, PathNoSlash, Duplicate,
// BadKindConfig) in one place that both surfaces can reuse.
package gregalemanifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/onebox-faas/faas/pkg/sched"
)

// TriggerKind is the closed vocabulary for `triggers[].kind`. PR-C
// shipped only `cron`; ADR-0NN widens it to six values to cover the
// five broker-pulled event-source-mapping kinds plus the in-platform
// queue/delayed_task merger. Adding a new kind requires (a) appending
// a constant here, (b) widening the `kind` CHECK on the
// `triggers` table in a follow-up migration (00267's CHECK already
// covers all six values, so the DB is forward-compatible with this
// manifest schema), and (c) adding the per-kind validator below.
// The CLI and the apid server-side validator both reject unknown
// kinds with the same upgrade-me message.
type TriggerKind string

const (
	// TriggerKindCron — synthetic wake on a cron schedule (ADR-090).
	// Mirrors the legacy `api.PlanCron` resource on the wire. The
	// kind is the only one that carries `schedule` + `path`; all
	// five non-cron kinds read their schedule semantics from the
	// upstream broker.
	TriggerKindCron TriggerKind = "cron"
	// TriggerKindKafka — Kafka consumer-group poll (issue #757).
	// Config schema: KafkaConfig{Brokers, Topic, Group}.
	TriggerKindKafka TriggerKind = "kafka"
	// TriggerKindNATS — NATS JetStream durable consumer (issue #757).
	// Config schema: NATSConfig{URL, Stream, Subject, Durable}.
	TriggerKindNATS TriggerKind = "nats"
	// TriggerKindRedisStreams — Redis XReadGroup consumer
	// (issue #757). Config schema: RedisStreamsConfig{Addr, Stream,
	// Group, Consumer}.
	TriggerKindRedisStreams TriggerKind = "redis_streams"
	// TriggerKindSQSCompat — long-poll the in-platform SQS-compatible
	// HTTP queue (issue #757). Config schema:
	// SQSCompatConfig{QueueURL, LongPollSecs}.
	TriggerKindSQSCompat TriggerKind = "sqs_compat"
	// TriggerKindQueue — in-platform queue / delayed_task merger
	// (issue #757, ADR-0NN). The platform's own `invocations` rows
	// with source IN ('queue','delayed_task') become a Trigger.
	// Config schema: QueueConfig{Mode}.
	TriggerKindQueue TriggerKind = "queue"
)

// Trigger is one entry under `triggers:`. PR-C ships cron-only fields;
// ADR-0NN adds the broker-pulled EventSourceMapping shape (Slug +
// BatchSizeMax + BatchWindowMs + MaxAttempts + Config) without
// breaking the existing cron path — `Schedule` and `Path` stay on the
// struct because they're required for kind=cron and a no-op for every
// other kind.
//
// `Enabled` is a pointer so the YAML decoder can distinguish "absent"
// from "explicit false" — the spec is "absent → true" (a trigger with
// no `enabled:` line is enabled).
type Trigger struct {
	Kind TriggerKind `yaml:"kind"`
	App  string      `yaml:"app"`
	Slug string      `yaml:"slug,omitempty"`
	// Schedule + Path are cron-only fields. They are required for
	// kind=cron and ignored for every other kind (the broker pulls
	// on its own cadence; the runner doesn't know how to map a
	// broker offset to a `/cleanup` HTTP path).
	Schedule string `yaml:"schedule,omitempty"`
	Path     string `yaml:"path,omitempty"`
	// BatchSizeMax + BatchWindowMs + MaxAttempts match the SQL CHECK
	// range on `triggers` (migration 00267): batch_size_max ∈
	// [1, 5000], batch_window_ms ∈ [10, 600000], max_attempts ∈
	// [1, 25]. The per-plan ceilings (pkg/api/Plan.TriggerBatchSizeMax
	// etc.) cap these BELOW the SQL ceiling — a Hobby customer asking
	// for 500 hits trigger_batch_size_too_long at the apid layer,
	// not a SQL 23514. Zero values mean "use the plan default" (the
	// SQL DEFAULTs to 64 / 1000 / 5).
	BatchSizeMax  int `yaml:"batch_size_max,omitempty"`
	BatchWindowMs int `yaml:"batch_window_ms,omitempty"`
	MaxAttempts   int `yaml:"max_attempts,omitempty"`
	// Config is the per-kind JSON object. Validated strictly per
	// kind in Validate(). Absent Config defaults to `{}` so a
	// bare trigger entry validates against the per-kind zero-value
	// default (which is itself a validation error — every non-cron
	// kind requires at least one non-empty field).
	Config  map[string]any `yaml:"config,omitempty"`
	Enabled *bool          `yaml:"enabled,omitempty"`
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

// KafkaConfig is the per-kind config for kind=kafka (issue #757).
// Brokers is a list of host:port pairs; Topic is the consumer-group
// subscription target; Group is the durable consumer-group ID.
type KafkaConfig struct {
	Brokers []string `json:"brokers"`
	Topic   string   `json:"topic"`
	Group   string   `json:"group"`
}

// NATSConfig is the per-kind config for kind=nats (issue #757). URL
// is the nats:// or tls:// endpoint; Stream is the JetStream stream
// name; Subject is the filter pattern (`events.>`); Durable is the
// durable consumer name.
type NATSConfig struct {
	URL     string `json:"url"`
	Stream  string `json:"stream"`
	Subject string `json:"subject"`
	Durable string `json:"durable"`
}

// RedisStreamsConfig is the per-kind config for kind=redis_streams
// (issue #757). Addr is host:port; Stream is the XReadGroup stream
// name; Group is the consumer group; Consumer is the per-instance
// consumer name (default the trigger slug).
type RedisStreamsConfig struct {
	Addr     string `json:"addr"`
	Stream   string `json:"stream"`
	Group    string `json:"group"`
	Consumer string `json:"consumer,omitempty"`
}

// SQSCompatConfig is the per-kind config for kind=sqs_compat (issue
// #757). QueueURL is the in-platform HTTP queue endpoint
// (`http://faas-queue:9090/queues/<name>`); LongPollSecs is the
// wait-time parameter (1–20; the platform caps at 20 per the
// AWS long-poll ceiling).
type SQSCompatConfig struct {
	QueueURL     string `json:"queue_url"`
	LongPollSecs int    `json:"long_poll_secs,omitempty"`
}

// QueueConfig is the per-kind config for kind=queue (issue #757). Mode
// selects which in-platform source to bind: "queue" for the per-app
// FIFO queue (invocations.source='queue') or "delayed_task" for the
// delayed-task surface (invocations.source='delayed_task').
type QueueConfig struct {
	Mode string `json:"mode"`
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
// upgrade-me message), then per-kind config (so the customer sees
// the specific field that's wrong rather than a generic "bad
// config"), then schedule (the cron-specific legacy check), then
// path + app + duplicates. The duplicate check is last because it's
// the most expensive and only meaningful when the per-entry checks
// pass.
//
// ADR-0NN extends the switch from one case (cron) to six; the cron
// path is byte-for-byte identical to PR-C, the five new kinds each
// validate their Config via decodeAndValidateConfig below.
func (m *Manifest) Validate() error {
	if m == nil {
		return nil
	}
	seen := make(map[triggerKey]struct{}, len(m.Triggers))
	for i, t := range m.Triggers {
		switch t.Kind {
		case TriggerKindCron,
			TriggerKindKafka,
			TriggerKindNATS,
			TriggerKindRedisStreams,
			TriggerKindSQSCompat,
			TriggerKindQueue:
			// fall through to per-kind validation below
		case "":
			return fmt.Errorf("trigger[%d]: missing kind (want one of %s)", i, supportedKindsList())
		default:
			return fmt.Errorf("trigger[%d]: unsupported trigger kind %q (supported kinds: %s)",
				i, t.Kind, supportedKindsList())
		}
		// App is universal — every kind binds to one app.
		if t.App == "" {
			return fmt.Errorf("trigger[%d]: app is required", i)
		}
		// Slug is required for non-cron kinds (the cron path derives
		// its dedupe key from (app, schedule, path) so it doesn't
		// need an explicit slug). The slug surfaces in the wire
		// path `/_triggers/<kind>/<slug>` so it must be DNS-safe.
		if t.Kind != TriggerKindCron {
			if t.Slug == "" {
				return fmt.Errorf("trigger[%d]: slug is required for kind=%q", i, t.Kind)
			}
			if !isDNSSafeSlug(t.Slug) {
				return fmt.Errorf("trigger[%d]: slug %q must match [a-z0-9-]+", i, t.Slug)
			}
		}
		// Per-kind config check. Cron's Config is the legacy
		// (schedule, path) pair; the five new kinds validate their
		// typed Config map.
		if err := t.validateKindConfig(i); err != nil {
			return err
		}
		// Batch/window/attempts ranges mirror the SQL CHECK on the
		// `triggers` table (migration 00267). A 0 value is "use the
		// SQL DEFAULT" (64 / 1000 / 5) — strictly speaking the SQL
		// CHECK rejects 0 for batch_size_max / batch_window_ms /
		// max_attempts, so the manifest validator surfaces the
		// customer-facing error rather than letting the row insert
		// fail at the DB layer. The cron kind ignores these fields.
		if t.Kind != TriggerKindCron {
			if t.BatchSizeMax != 0 && (t.BatchSizeMax < 1 || t.BatchSizeMax > 5000) {
				return fmt.Errorf("trigger[%d]: batch_size_max=%d out of range [1, 5000]", i, t.BatchSizeMax)
			}
			if t.BatchWindowMs != 0 && (t.BatchWindowMs < 10 || t.BatchWindowMs > 600000) {
				return fmt.Errorf("trigger[%d]: batch_window_ms=%d out of range [10, 600000]", i, t.BatchWindowMs)
			}
			if t.MaxAttempts != 0 && (t.MaxAttempts < 1 || t.MaxAttempts > 25) {
				return fmt.Errorf("trigger[%d]: max_attempts=%d out of range [1, 25]", i, t.MaxAttempts)
			}
		}
		k := triggerKey{app: t.App, kind: t.Kind, schedule: t.Schedule, path: t.Path, slug: t.Slug}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("trigger[%d]: duplicate (app, kind, slug) — %q / %q / %q",
				i, t.App, t.Kind, t.Slug)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// supportedKindsList returns the human-readable comma-separated list
// of supported TriggerKind values for inclusion in upgrade-me error
// messages. Order is intentionally stable (cron first — the legacy
// shape — then alphabetical) so a customer grep'ing for "kafka" in
// the error message finds it consistently.
func supportedKindsList() string {
	return "cron, kafka, nats, redis_streams, sqs_compat, queue"
}

// validateKindConfig runs the per-kind config check. The cron kind's
// schedule + path pair is the legacy PR-C contract; the five new
// kinds validate their typed config struct.
//
// We marshal the YAML-decoded Config map back to JSON to feed the
// std json.Unmarshal — round-tripping through bytes is the cheapest
// path that lets both surfaces (CLI decoder + apid server-side
// validator) reuse the same shape with no custom YAML→JSON shim.
// The cost is one extra allocation per trigger per manifest apply;
// negligible relative to the apid round-trip.
func (t Trigger) validateKindConfig(idx int) error {
	switch t.Kind {
	case TriggerKindCron:
		if _, err := sched.ParseSchedule(t.Schedule); err != nil {
			return fmt.Errorf("trigger[%d]: bad schedule %q: %w", idx, t.Schedule, err)
		}
		if !strings.HasPrefix(t.Path, "/") {
			return fmt.Errorf("trigger[%d]: path must start with '/' (got %q)", idx, t.Path)
		}
		return nil
	case TriggerKindKafka:
		var c KafkaConfig
		if err := decodeInto(t.Config, &c); err != nil {
			return fmt.Errorf("trigger[%d]: bad kafka config: %w", idx, err)
		}
		if len(c.Brokers) == 0 {
			return fmt.Errorf("trigger[%d]: kafka config requires non-empty brokers", idx)
		}
		if c.Topic == "" {
			return fmt.Errorf("trigger[%d]: kafka config requires non-empty topic", idx)
		}
		if c.Group == "" {
			return fmt.Errorf("trigger[%d]: kafka config requires non-empty group", idx)
		}
		return nil
	case TriggerKindNATS:
		var c NATSConfig
		if err := decodeInto(t.Config, &c); err != nil {
			return fmt.Errorf("trigger[%d]: bad nats config: %w", idx, err)
		}
		if c.URL == "" {
			return fmt.Errorf("trigger[%d]: nats config requires non-empty url", idx)
		}
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "nats" && u.Scheme != "tls") || u.Host == "" {
			return fmt.Errorf("trigger[%d]: nats url must be nats:// or tls:// with a host (got %q)", idx, c.URL)
		}
		if c.Stream == "" {
			return fmt.Errorf("trigger[%d]: nats config requires non-empty stream", idx)
		}
		if c.Subject == "" {
			return fmt.Errorf("trigger[%d]: nats config requires non-empty subject", idx)
		}
		if c.Durable == "" {
			return fmt.Errorf("trigger[%d]: nats config requires non-empty durable", idx)
		}
		return nil
	case TriggerKindRedisStreams:
		var c RedisStreamsConfig
		if err := decodeInto(t.Config, &c); err != nil {
			return fmt.Errorf("trigger[%d]: bad redis_streams config: %w", idx, err)
		}
		if c.Addr == "" {
			return fmt.Errorf("trigger[%d]: redis_streams config requires non-empty addr", idx)
		}
		if c.Stream == "" {
			return fmt.Errorf("trigger[%d]: redis_streams config requires non-empty stream", idx)
		}
		if c.Group == "" {
			return fmt.Errorf("trigger[%d]: redis_streams config requires non-empty group", idx)
		}
		return nil
	case TriggerKindSQSCompat:
		var c SQSCompatConfig
		if err := decodeInto(t.Config, &c); err != nil {
			return fmt.Errorf("trigger[%d]: bad sqs_compat config: %w", idx, err)
		}
		if c.QueueURL == "" {
			return fmt.Errorf("trigger[%d]: sqs_compat config requires non-empty queue_url", idx)
		}
		u, err := url.Parse(c.QueueURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("trigger[%d]: sqs_compat queue_url must be http:// or https:// with a host (got %q)", idx, c.QueueURL)
		}
		if c.LongPollSecs != 0 && (c.LongPollSecs < 1 || c.LongPollSecs > 20) {
			return fmt.Errorf("trigger[%d]: sqs_compat long_poll_secs=%d out of range [1, 20]", idx, c.LongPollSecs)
		}
		return nil
	case TriggerKindQueue:
		var c QueueConfig
		if err := decodeInto(t.Config, &c); err != nil {
			return fmt.Errorf("trigger[%d]: bad queue config: %w", idx, err)
		}
		switch c.Mode {
		case "queue", "delayed_task":
			return nil
		default:
			return fmt.Errorf("trigger[%d]: queue config mode %q not in {queue, delayed_task}", idx, c.Mode)
		}
	}
	// Unreachable: the outer switch in Validate already rejected
	// unknown kinds. Returning nil here keeps the linter quiet and
	// the function total.
	return nil
}

// decodeInto round-trips the YAML-decoded Config map (typed as
// map[string]any) through JSON to feed json.Unmarshal. The round-trip
// is necessary because the YAML decoder uses gopkg.in/yaml.v3 which
// returns map[interface{}]interface{} for nested maps unless we
// decode into the typed struct directly via YAML — but the typed
// struct lives in both the CLI loader AND the apid server-side
// validator (cmd/apid/scan_service.go) where the input is already
// JSON from the wire, so a single json-based path keeps both surfaces
// symmetric. An empty/nil Config decodes as the zero value of the
// target struct (no error), which surfaces as the per-field required
// errors below.
func decodeInto(src map[string]any, dst any) error {
	if len(src) == 0 {
		return nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// isDNSSafeSlug mirrors the wire-path slug rules: lowercase
// alphanumeric + dashes, must start with a letter, ≤63 chars (the
// DNS label ceiling). Anything outside this set surfaces a manifest
// validation error so the customer sees the issue at `gregale
// deploy` time rather than at apid-request time.
func isDNSSafeSlug(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	if !isLowerAlpha(s[0]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !isLowerAlpha(c) && !isDigit(c) && c != '-' {
			return false
		}
	}
	return true
}

func isLowerAlpha(b byte) bool { return b >= 'a' && b <= 'z' }
func isDigit(b byte) bool      { return b >= '0' && b <= '9' }

// triggerKey is the dedupe primitive. Five fields because two
// triggers of the same kind on the same app with the same slug are
// the same resource (the SQL UNIQUE (app_id, slug) constraint on the
// `triggers` table — migration 00267). Cron is special-cased: it
// derives its slug implicitly from the (schedule, path) tuple, so the
// dedupe key for kind=cron keeps (schedule, path) and zeroes the
// slug, while the five non-cron kinds rely on the explicit slug.
// Mixing the two shapes in one key tuple would let a cron "*/5 * * * *
// /tick" collide with a kafka trigger whose slug happened to be
// "*/5 * * * * /tick" — instead we keep the two distinct shapes in
// the same tuple with (kind, slug) acting as the discriminating
// fields for non-cron kinds.
type triggerKey struct {
	app      string
	kind     TriggerKind
	schedule string
	path     string
	slug     string
}
