package api

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestManifestDefaults(t *testing.T) {
	m := AppManifest{Entrypoint: []string{"/app/server"}}
	if m.EffectivePort() != DefaultAppPort {
		t.Errorf("port default = %d, want %d", m.EffectivePort(), DefaultAppPort)
	}
	if m.EffectiveUser() != DefaultAppUser {
		t.Errorf("user default = %q, want %q", m.EffectiveUser(), DefaultAppUser)
	}
	if m.EffectiveWorkingDir() != "/" {
		t.Errorf("workdir default = %q, want /", m.EffectiveWorkingDir())
	}
}

func TestManifestExplicitValuesWin(t *testing.T) {
	m := AppManifest{
		Entrypoint: []string{"/app/server"},
		Port:       8080,
		User:       "nobody",
		WorkingDir: "/srv",
	}
	if m.EffectivePort() != 8080 {
		t.Errorf("port override = %d, want 8080", m.EffectivePort())
	}
	if m.EffectiveUser() != "nobody" {
		t.Errorf("user override = %q", m.EffectiveUser())
	}
	if m.EffectiveWorkingDir() != "/srv" {
		t.Errorf("workdir override = %q", m.EffectiveWorkingDir())
	}
}

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name string
		m    AppManifest
		ok   bool
	}{
		{"valid", AppManifest{Entrypoint: []string{"node", "index.js"}}, true},
		{"empty entrypoint", AppManifest{}, false},
		{"empty argv0", AppManifest{Entrypoint: []string{""}}, false},
		{"bad port", AppManifest{Entrypoint: []string{"x"}, Port: 70000}, false},
		{"neg port", AppManifest{Entrypoint: []string{"x"}, Port: -1}, false},
		// Issue #460 / ADR-053 — env_secrets refs (PR-B wiring). Ref names
		// match ^[A-Z][A-Z0-9_]*$ (same grammar as pkg/api/dto.go's apid
		// validation, mirrored to keep the manifest contract self-contained).
		{"env_secrets well-formed", AppManifest{
			Entrypoint: []string{"x"},
			EnvSecrets: map[string]string{"DB_URL": "secret:DB_URL"},
		}, true},
		{"env_secrets missing prefix", AppManifest{
			Entrypoint: []string{"x"},
			EnvSecrets: map[string]string{"DB_URL": "plaintext"},
		}, false},
		{"env_secrets bad ref name (lowercase)", AppManifest{
			Entrypoint: []string{"x"},
			EnvSecrets: map[string]string{"DB_URL": "secret:lowercase"},
		}, false},
		{"env_secrets empty value", AppManifest{
			Entrypoint: []string{"x"},
			EnvSecrets: map[string]string{"DB_URL": ""},
		}, false},
		{"env_secrets empty map", AppManifest{
			Entrypoint: []string{"x"},
			EnvSecrets: map[string]string{},
		}, true},
		// M-1 (ADR-136) — StopGracePeriod bounded at the manifest side
		// to keep the platform's tail-drain budget sane. The 5-minute
		// cap is gross; per-plan tightening lands in M-2.
		{"stop_grace_period zero ok", AppManifest{
			Entrypoint: []string{"x"}, StopGracePeriod: 0,
		}, true},
		{"stop_grace_period under cap ok", AppManifest{
			Entrypoint: []string{"x"}, StopGracePeriod: 30 * time.Second,
		}, true},
		{"stop_grace_period at cap ok", AppManifest{
			Entrypoint: []string{"x"}, StopGracePeriod: 5 * time.Minute,
		}, true},
		{"stop_grace_period over cap rejected", AppManifest{
			Entrypoint: []string{"x"}, StopGracePeriod: 5*time.Minute + time.Second,
		}, false},
		{"stop_grace_period negative rejected", AppManifest{
			Entrypoint: []string{"x"}, StopGracePeriod: -1 * time.Second,
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("Validate() err=%v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestManifestRoundTrip(t *testing.T) {
	in := AppManifest{
		Entrypoint: []string{"node", "server.js"},
		Env:        map[string]string{"NODE_ENV": "production"},
		EnvSecrets: map[string]string{"DB_URL": "secret:DB_URL", "API_KEY": "secret:API_KEY"},
		Port:       3000,
		Healthz:    "/healthz",
	}
	var buf bytes.Buffer
	if err := WriteManifest(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadManifest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.Entrypoint[1] != "server.js" || out.Port != 3000 || out.Env["NODE_ENV"] != "production" {
		t.Errorf("round trip mismatch: %+v", out)
	}
	if out.EnvSecrets["DB_URL"] != "secret:DB_URL" || out.EnvSecrets["API_KEY"] != "secret:API_KEY" {
		t.Errorf("env_secrets round trip mismatch: %+v", out.EnvSecrets)
	}
}

func TestReadManifestRejectsInvalid(t *testing.T) {
	if _, err := ReadManifest(strings.NewReader(`{"port":3000}`)); err == nil {
		t.Error("manifest with no entrypoint should fail validation on read")
	}
}

func TestErrAppLayerTooLarge(t *testing.T) {
	l := MustLimitsFor(PlanFree) // 256 MB cap
	p := ErrAppLayerTooLarge(l, 300*1024*1024)
	if p.Code != CodeAppLayerTooBig {
		t.Errorf("code = %q", p.Code)
	}
	if p.Limit == nil || *p.Limit != 256*1024*1024 {
		t.Errorf("limit not set to plan cap bytes: %v", p.Limit)
	}
	if !strings.Contains(p.Detail, "256 MB") {
		t.Errorf("detail should name the cap: %q", p.Detail)
	}
}
