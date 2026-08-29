package oci

import (
	"bytes"
	"os"
	"testing"
)

// TestParseConfig_FlatOnly asserts the registry-flat (Docker v2) path
// resolves Cmd, Env, WorkingDir, User when no nested-`config` envelope
// is present. Both ParseConfig (rich struct, package-external) and
// parseImageConfig (consumer projection) must agree.
func TestParseConfig_FlatOnly(t *testing.T) {
	b := readFixture(t, "docker_v2_flat.json")

	cfg, err := ParseConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got, want := cfg.Cmd[0], "/bin/sh"; got != want {
		t.Errorf("Config.Cmd[0] = %q; want %q", got, want)
	}
	if got, want := cfg.User, "1001"; got != want {
		t.Errorf("Config.User = %q; want %q", got, want)
	}
	if got, want := cfg.WorkingDir, "/app"; got != want {
		t.Errorf("Config.WorkingDir = %q; want %q", got, want)
	}
	if got, want := len(cfg.Env), 3; got != want {
		t.Errorf("Config.Env len = %d; want %d", got, want)
	}

	img, err := parseImageConfig(b)
	if err != nil {
		t.Fatalf("parseImageConfig: %v", err)
	}
	if got, want := img.Cmd[0], "/bin/sh"; got != want {
		t.Errorf("ImageConfig.Cmd[0] = %q; want %q", got, want)
	}
	if got, want := img.Env["FOO"], "bar"; got != want {
		t.Errorf("ImageConfig.Env[FOO] = %q; want %q", got, want)
	}
	if got, want := img.Env["LANG"], "C.UTF-8"; got != want {
		t.Errorf("ImageConfig.Env[LANG] = %q; want %q", got, want)
	}
	if got, want := img.WorkingDir, "/app"; got != want {
		t.Errorf("ImageConfig.WorkingDir = %q; want %q", got, want)
	}
}

// TestParseConfig_NestedOCIOnly asserts the OCI nested-`config`
// envelope is consumed when no flat fields are present. Both
// parsers must surface User / WorkingDir / DiffIDs from the envelope.
func TestParseConfig_NestedOCIOnly(t *testing.T) {
	b := readFixture(t, "nested_oci.json")

	cfg, err := ParseConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got, want := cfg.Cmd[0], "/app/server"; got != want {
		t.Errorf("Config.Cmd[0] = %q; want %q", got, want)
	}
	if got, want := cfg.User, "65532"; got != want {
		t.Errorf("Config.User = %q; want %q (from nested envelope)", got, want)
	}
	if got, want := cfg.WorkingDir, "/workspace"; got != want {
		t.Errorf("Config.WorkingDir = %q; want %q (from nested envelope)", got, want)
	}
	if got, want := len(cfg.DiffIDs), 2; got != want {
		t.Errorf("Config.DiffIDs len = %d; want %d", got, want)
	}
}

// TestParseConfig_BothPreferFlat asserts the precedence rule documented
// in ADR-136 §Decision 1: flat fields win when both envelopes are
// present; nested fields fill gaps only when the flat is empty/absent.
func TestParseConfig_BothPreferFlat(t *testing.T) {
	b := readFixture(t, "flat_and_nested_prefer_flat.json")

	cfg, err := ParseConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got, want := cfg.Cmd[0], "/flat/cmd"; got != want {
		t.Errorf("Config.Cmd[0] = %q; want %q (flat precedence)", got, want)
	}
	if got, want := cfg.Env[0], "FROM=flat"; got != want {
		t.Errorf("Config.Env[0] = %q; want %q (flat precedence)", got, want)
	}
	if got, want := cfg.Entrypoint[0], "/flat/entrypoint"; got != want {
		t.Errorf("Config.Entrypoint[0] = %q; want %q (flat precedence)", got, want)
	}
	if got, want := cfg.WorkingDir, "/nested/dir"; got != want {
		t.Errorf("Config.WorkingDir = %q; want %q (nested fallback for missing flat)", got, want)
	}
	if got, want := cfg.User, "nested-user"; got != want {
		t.Errorf("Config.User = %q; want %q (nested fallback for missing flat)", got, want)
	}
}

// TestParseConfig_EmptyConfigOK asserts an image config that has no
// usable fields doesn't fail — it surfaces empty/zero values for both
// parsers. Real registries occasionally emit near-empty configs for
// minimal images; the caller's Validate() decides whether to reject.
func TestParseConfig_EmptyConfigOK(t *testing.T) {
	b := readFixture(t, "empty_config.json")

	cfg, err := ParseConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Cmd) != 0 || len(cfg.Env) != 0 || len(cfg.Entrypoint) != 0 {
		t.Errorf("empty Config: expected all fields zero; got %+v", cfg)
	}

	img, err := parseImageConfig(b)
	if err != nil {
		t.Fatalf("parseImageConfig: %v", err)
	}
	if len(img.Cmd) != 0 || len(img.Env) != 0 {
		t.Errorf("empty ImageConfig: expected all fields zero; got %+v", img)
	}
}

// TestParseImageConfig_WeirdEnvKeysPreserved closes the env-flatten
// regression reported by ADR-136 §Decision 2: `=VALUE` keys (empty
// key, real value) were silently dropped before commit 3; bare entries
// (no `=` at all) were also dropped. Both must survive.
func TestParseImageConfig_WeirdEnvKeysPreserved(t *testing.T) {
	b := readFixture(t, "weird_env.json")

	img, err := parseImageConfig(b)
	if err != nil {
		t.Fatalf("parseImageConfig: %v", err)
	}
	if v, ok := img.Env[""]; !ok || v != "PATH" {
		t.Errorf(`ImageConfig.Env[""] = (%q, %v); want (PATH, true)`, v, ok)
	}
	if v, ok := img.Env["NORMAL"]; !ok || v != "value" {
		t.Errorf(`ImageConfig.Env["NORMAL"] = (%q, %v); want (value, true)`, v, ok)
	}
	if v, ok := img.Env["EMPTY"]; !ok || v != "" {
		t.Errorf(`ImageConfig.Env["EMPTY"] = (%q, %v); want ("", true)`, v, ok)
	}
	if v, ok := img.Env["EQUALS"]; !ok || v != "foo=bar=baz" {
		t.Errorf(`ImageConfig.Env["EQUALS"] = (%q, %v); want (foo=bar=baz, true)`, v, ok)
	}
}

// TestParseConfig_UnsupportedRootFS rejects rootfs.type values other
// than "layers" — both parsers share the validator, so both must fail.
func TestParseConfig_UnsupportedRootFS(t *testing.T) {
	b := []byte(`{"rootfs": {"type": "btrfs", "diff_ids": []}}`)

	if _, err := ParseConfig(bytes.NewReader(b)); err == nil {
		t.Error("ParseConfig accepted rootfs.type=btrfs; want error")
	}
	if _, err := parseImageConfig(b); err == nil {
		t.Error("parseImageConfig accepted rootfs.type=btrfs; want error")
	}
}

// TestParseConfig_MalformedJSON surfaces decode errors verbatim.
func TestParseConfig_MalformedJSON(t *testing.T) {
	for _, name := range []string{"{", "{ \"Cmd\":", "[1,2,3]"} {
		if _, err := ParseConfig(bytes.NewReader([]byte(name))); err == nil {
			t.Errorf("ParseConfig accepted malformed input %q; want error", name)
		}
	}
}

// readFixture loads a JSON test fixture from pkg/oci/testdata/parse/.
// Files are committed to the repo (no embed) so the canonical source
// is the same file the parser consumes in production.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/parse/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}
