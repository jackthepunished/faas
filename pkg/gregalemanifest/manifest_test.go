package gregalemanifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_NoManifest(t *testing.T) {
	dir := t.TempDir()
	m, ok, err := Load(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (no manifest present)")
	}
	if m != nil {
		t.Errorf("m = %+v, want nil", m)
	}
}

func TestLoad_YAMLPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gregale.yaml"), []byte("triggers: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, ok, err := Load(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if len(m.Triggers) != 0 {
		t.Errorf("triggers = %+v, want empty", m.Triggers)
	}
}

func TestLoad_YMLFallback(t *testing.T) {
	// .yml is the alternate extension. Both .yaml and .yml are
	// accepted; .yaml wins when both are present (load order).
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gregale.yml"), []byte("triggers: []\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, ok, err := Load(dir)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Errorf("ok = false, want true (yml is a valid fallback)")
	}
}

func TestLoad_TOMLRejectedExplicitly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gregale.toml"), []byte("[triggers]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("err = nil, want explicit TOML rejection")
	}
	if !strings.Contains(err.Error(), "TOML manifests are not supported") {
		t.Errorf("err = %q, want TOML rejection copy", err)
	}
}

func TestLoad_StrictUnknownField(t *testing.T) {
	// Typo'd `trigger:` (singular) under root. With KnownFields(true)
	// the decoder rejects the unknown top-level key, surfacing the
	// typo at load time instead of silently shipping a no-op.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gregale.yaml"),
		[]byte("trigger:\n  - kind: cron\n    app: my-api\n    schedule: \"0 3 * * *\"\n    path: /cleanup\n"),
		0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := Load(dir)
	if err == nil {
		t.Fatal("err = nil, want strict-decode error on unknown field")
	}
	if !strings.Contains(err.Error(), "field trigger not found") {
		t.Errorf("err = %q, want strict-decode message", err)
	}
}

func TestValidate_HappyPath(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: TriggerKindCron, App: "my-api", Schedule: "0 3 * * *", Path: "/cleanup"},
		{Kind: TriggerKindCron, App: "my-api", Schedule: "*/5 * * * *", Path: "/tick", Enabled: ptrBool(false)},
	}}
	if err := m.Validate(); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
}

func TestValidate_UnknownKind(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: "queue", App: "x", Schedule: "0 3 * * *", Path: "/y"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "unsupported trigger kind \"queue\"") {
		t.Errorf("err = %v, want unsupported-kind message", err)
	}
}

func TestValidate_MissingKind(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{App: "x", Schedule: "0 3 * * *", Path: "/y"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "missing kind") {
		t.Errorf("err = %v, want missing-kind message", err)
	}
}

func TestValidate_BadSchedule(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: TriggerKindCron, App: "x", Schedule: "not a cron", Path: "/y"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "bad schedule") {
		t.Errorf("err = %v, want bad-schedule message", err)
	}
}

func TestValidate_PathNoSlash(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: TriggerKindCron, App: "x", Schedule: "0 3 * * *", Path: "cleanup"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "path must start with '/'") {
		t.Errorf("err = %v, want path-must-start-with-slash message", err)
	}
}

func TestValidate_MissingApp(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: TriggerKindCron, Schedule: "0 3 * * *", Path: "/y"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "app is required") {
		t.Errorf("err = %v, want app-required message", err)
	}
}

func TestValidate_DuplicateTriple(t *testing.T) {
	m := &Manifest{Triggers: []Trigger{
		{Kind: TriggerKindCron, App: "x", Schedule: "0 3 * * *", Path: "/y"},
		{Kind: TriggerKindCron, App: "x", Schedule: "0 3 * * *", Path: "/y"},
	}}
	err := m.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want duplicate-triple message", err)
	}
}

func TestIsEnabled(t *testing.T) {
	cases := []struct {
		name string
		t    Trigger
		want bool
	}{
		{"nil pointer defaults to true", Trigger{}, true},
		{"explicit true", Trigger{Enabled: ptrBool(true)}, true},
		{"explicit false", Trigger{Enabled: ptrBool(false)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.t.IsEnabled(); got != c.want {
				t.Errorf("IsEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }
