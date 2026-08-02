package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// testSidecarLimits returns a Limits struct with a generous
// EnvValueMaxBytes for the dto-sidecar unit tests. Production
// limits are smaller (4 KB on Hobby; 32 KB on Pro+); the test
// only needs the field to be non-zero so the per-value byte cap
// path runs. A 1 MiB cap is comfortably above every test payload.
func testSidecarLimits() Limits {
	return Limits{
		Plan:             PlanHobby,
		RAMMB:            256,
		EnvValueMaxBytes: 1 << 20,
	}
}

func TestSidecar_Validate_Accepts(t *testing.T) {
	limits := testSidecarLimits()
	essTrue := true
	cases := []struct {
		name string
		s    Sidecar
	}{
		{
			name: "init-only",
			s: Sidecar{
				Name:      "migrator",
				Image:     "ghcr.io/me/migrator@sha256:0000000000000000000000000000000000000000000000000000000000000001",
				Type:      SidecarTypeInit,
				Cmd:       []string{"--to", "head"},
				Env:       map[string]string{"DB_URL": "postgres://x"},
				Port:      0,
				RamMB:     64,
				Essential: &essTrue,
			},
		},
		{
			name: "sidecar-only",
			s: Sidecar{
				Name:  "scraper",
				Image: "ghcr.io/me/scraper@sha256:0000000000000000000000000000000000000000000000000000000000000002",
				Type:  SidecarTypeSidecar,
				Port:  9090,
				RamMB: 32,
			},
		},
		{
			name: "minimal-port-ram-absent",
			s: Sidecar{
				Name:  "only",
				Image: "r/x@sha256:" + strings.Repeat("a", 64),
				Type:  SidecarTypeInit,
			},
		},
		{
			name: "with-underscore-and-dot-image",
			s: Sidecar{
				Name:  "complex",
				Image: "registry.example.com:5000/path/to/app_v1.2@sha256:" + strings.Repeat("f", 64),
				Type:  SidecarTypeSidecar,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := tc.s.Validate(limits); p != nil {
				t.Errorf("Validate(Accepts) = %v, want nil", p)
			}
		})
	}
}

func TestSidecar_Validate_Rejects(t *testing.T) {
	limits := testSidecarLimits()
	essTrue := true
	essFalse := false
	goodImage := "ghcr.io/me/x@sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name    string
		s       Sidecar
		wantSub string // substring expected in the problem title or detail
	}{
		{
			name:    "name-uppercase",
			s:       Sidecar{Name: "Migrator", Image: goodImage, Type: SidecarTypeInit},
			wantSub: "sidecar name",
		},
		{
			name:    "name-empty",
			s:       Sidecar{Name: "", Image: goodImage, Type: SidecarTypeInit},
			wantSub: "sidecar name",
		},
		{
			name:    "name-leading-dash",
			s:       Sidecar{Name: "-migrator", Image: goodImage, Type: SidecarTypeInit},
			wantSub: "sidecar name",
		},
		{
			name:    "name-too-long",
			s:       Sidecar{Name: strings.Repeat("a", 64), Image: goodImage, Type: SidecarTypeInit},
			wantSub: "sidecar name",
		},
		{
			name:    "image-by-tag",
			s:       Sidecar{Name: "ok", Image: "ghcr.io/me/x:latest", Type: SidecarTypeInit},
			wantSub: "Invalid sidecar image",
		},
		{
			name:    "image-missing-digest",
			s:       Sidecar{Name: "ok", Image: "ghcr.io/me/x", Type: SidecarTypeInit},
			wantSub: "Invalid sidecar image",
		},
		{
			name:    "image-uppercase-hex",
			s:       Sidecar{Name: "ok", Image: "r/x@sha256:" + strings.Repeat("A", 64), Type: SidecarTypeInit},
			wantSub: "Invalid sidecar image",
		},
		{
			name:    "image-short-digest",
			s:       Sidecar{Name: "ok", Image: "r/x@sha256:" + strings.Repeat("a", 63), Type: SidecarTypeInit},
			wantSub: "Invalid sidecar image",
		},
		{
			name:    "type-bogus",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarType("init2")},
			wantSub: "Invalid sidecar type",
		},
		{
			name:    "type-empty",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: ""},
			wantSub: "Invalid sidecar type",
		},
		{
			name:    "cmd-empty-element",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeInit, Cmd: []string{"--to", ""}},
			wantSub: "every argv element",
		},
		{
			name: "env-value-too-long",
			s: Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeInit,
				Env: map[string]string{"BIG": strings.Repeat("x", 2<<20)}},
			wantSub: "value is",
		},
		{
			name:    "port-65536",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, Port: 65536},
			wantSub: "sidecar port",
		},
		{
			name:    "port-negative",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, Port: -1},
			wantSub: "sidecar port",
		},
		{
			name:    "ram-below-floor",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, RamMB: 16},
			wantSub: "sidecar ram_mb",
		},
		{
			name:    "ram-above-ceiling",
			s:       Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, RamMB: 1024},
			wantSub: "sidecar ram_mb",
		},
		// essential true / false accepted; no error from Validate.
		{name: "essential-true-ok", s: Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, Essential: &essTrue}, wantSub: ""},
		{name: "essential-false-ok", s: Sidecar{Name: "ok", Image: goodImage, Type: SidecarTypeSidecar, Essential: &essFalse}, wantSub: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.s.Validate(limits)
			if tc.wantSub == "" {
				if p != nil {
					t.Errorf("Validate(Rejects[%s]) = %v, want nil", tc.name, p)
				}
				return
			}
			if p == nil {
				t.Errorf("Validate(Rejects[%s]) = nil, want error containing %q", tc.name, tc.wantSub)
				return
			}
			body := p.Title + " " + p.Detail
			if !strings.Contains(body, tc.wantSub) {
				t.Errorf("Validate(Rejects[%s]) detail = %q, want substring %q", tc.name, body, tc.wantSub)
			}
		})
	}
}

func TestSidecars_Validate_Accepts(t *testing.T) {
	limits := testSidecarLimits()
	goodImage := "ghcr.io/me/x@sha256:" + strings.Repeat("a", 64)
	goodImage2 := "ghcr.io/me/y@sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		name string
		ss   Sidecars
	}{
		{"empty", nil},
		{"empty-slice", Sidecars{}},
		{"init-only", Sidecars{
			{Name: "migrator", Image: goodImage, Type: SidecarTypeInit},
		}},
		{"sidecar-only", Sidecars{
			{Name: "scraper", Image: goodImage, Type: SidecarTypeSidecar},
		}},
		{"one-init-one-sidecar", Sidecars{
			{Name: "migrator", Image: goodImage, Type: SidecarTypeInit},
			{Name: "scraper", Image: goodImage2, Type: SidecarTypeSidecar},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := tc.ss.Validate(limits); p != nil {
				t.Errorf("Validate(Accepts[%s]) = %v, want nil", tc.name, p)
			}
		})
	}
}

func TestSidecars_Validate_Rejects(t *testing.T) {
	limits := testSidecarLimits()
	goodImage := "ghcr.io/me/x@sha256:" + strings.Repeat("a", 64)
	goodImage2 := "ghcr.io/me/y@sha256:" + strings.Repeat("b", 64)
	goodImage3 := "ghcr.io/me/z@sha256:" + strings.Repeat("c", 64)
	cases := []struct {
		name    string
		ss      Sidecars
		wantSub string
	}{
		{
			name: "three-sidecars-over-cap",
			ss: Sidecars{
				{Name: "a", Image: goodImage, Type: SidecarTypeInit},
				{Name: "b", Image: goodImage2, Type: SidecarTypeSidecar},
				{Name: "c", Image: goodImage3, Type: SidecarTypeInit},
			},
			wantSub: "Too many sidecars",
		},
		{
			name: "two-init-duplicate-type",
			ss: Sidecars{
				{Name: "a", Image: goodImage, Type: SidecarTypeInit},
				{Name: "b", Image: goodImage2, Type: SidecarTypeInit},
			},
			wantSub: "at most one sidecar of type",
		},
		{
			name: "two-sidecar-duplicate-type",
			ss: Sidecars{
				{Name: "a", Image: goodImage, Type: SidecarTypeSidecar},
				{Name: "b", Image: goodImage2, Type: SidecarTypeSidecar},
			},
			wantSub: "at most one sidecar of type",
		},
		{
			name: "duplicate-name",
			ss: Sidecars{
				{Name: "dup", Image: goodImage, Type: SidecarTypeInit},
				{Name: "dup", Image: goodImage2, Type: SidecarTypeSidecar},
			},
			wantSub: "appears more than once",
		},
		{
			name: "per-element-validation-propagates",
			ss: Sidecars{
				{Name: "ok", Image: goodImage, Type: SidecarTypeInit},
				{Name: "BAD-Name", Image: goodImage2, Type: SidecarTypeSidecar},
			},
			wantSub: "sidecar name",
		},
		{
			name: "per-element-image-tag-propagates",
			ss: Sidecars{
				{Name: "ok", Image: goodImage, Type: SidecarTypeInit},
				{Name: "ok2", Image: "r/x:latest", Type: SidecarTypeSidecar},
			},
			wantSub: "Invalid sidecar image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.ss.Validate(limits)
			if p == nil {
				t.Errorf("Validate(Rejects[%s]) = nil, want error containing %q", tc.name, tc.wantSub)
				return
			}
			body := p.Title + " " + p.Detail
			if !strings.Contains(body, tc.wantSub) {
				t.Errorf("Validate(Rejects[%s]) detail = %q, want substring %q", tc.name, body, tc.wantSub)
			}
		})
	}
}

// TestSidecar_JSONRoundTrip pins that the wire shape is stable:
// json.Marshal + json.Unmarshal preserves every field. PR-A's
// contract is the wire shape; if any field drifts between the
// SDK gen and the on-the-wire shape, this test catches it.
func TestSidecar_JSONRoundTrip(t *testing.T) {
	essTrue := true
	original := Sidecar{
		Name:      "migrator",
		Image:     "ghcr.io/me/x@sha256:" + strings.Repeat("a", 64),
		Type:      SidecarTypeInit,
		Cmd:       []string{"--to", "head"},
		Env:       map[string]string{"DB_URL": "postgres://x"},
		Port:      9090,
		RamMB:     64,
		Essential: &essTrue,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got Sidecar
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Name != original.Name {
		t.Errorf("Name round-trip: got %q, want %q", got.Name, original.Name)
	}
	if got.Image != original.Image {
		t.Errorf("Image round-trip: got %q, want %q", got.Image, original.Image)
	}
	if got.Type != original.Type {
		t.Errorf("Type round-trip: got %q, want %q", got.Type, original.Type)
	}
	if len(got.Cmd) != len(original.Cmd) {
		t.Errorf("Cmd length: got %d, want %d", len(got.Cmd), len(original.Cmd))
	}
	if got.Env["DB_URL"] != original.Env["DB_URL"] {
		t.Errorf("Env[DB_URL]: got %q, want %q", got.Env["DB_URL"], original.Env["DB_URL"])
	}
	if got.Port != original.Port {
		t.Errorf("Port: got %d, want %d", got.Port, original.Port)
	}
	if got.RamMB != original.RamMB {
		t.Errorf("RamMB: got %d, want %d", got.RamMB, original.RamMB)
	}
	if got.Essential == nil || *got.Essential != *original.Essential {
		t.Errorf("Essential: got %v, want %v", got.Essential, original.Essential)
	}
}
