package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// decodeForTest wraps json.Decoder with DisallowUnknownFields, mirroring
// what cmd/apid/server.go::decodeJSON does for the live handler. The
// frozen-fields invariant (ADR-053 §Decision 1) is enforced by this
// flag — every override shape beyond the six declared fields 400s.
func decodeForTest(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// TestCreateDeploymentOverrides_Validate is the table-driven pin for
// ADR-053 §Decision 1 validation. Every case corresponds to a rule in
// the §Decision 1 table; the case names mirror the rule titles so a
// regression in production maps 1:1 to a failing case.
//
// The validation rules pinned here:
//
//   - nil overrides -> nil problem (callers may pass nil for "no
//     override"; not an error).
//   - entrypoint: non-empty if present; every element non-empty.
//   - cmd: non-empty if present; every element non-empty.
//   - env + env_secrets count is capped by Limits.EnvVarsMax
//     (shared gate; ADR-045 §Decision 1 mirror).
//   - env key grammar per ValidateEnvKey (^[A-Z][A-Z0-9_]*$).
//   - env per-value byte cap per Limits.EnvValueMaxBytes.
//   - env_secrets value must start with "secret:" and the NAME must
//     match ^[A-Z][A-Z0-9_]*$.
//   - env_secrets per-value byte cap (the ref string length).
//   - port: 0 means absent; 1..65535 valid; anything else 400.
//   - healthcheck: path must start with "/"; interval/timeout/retries
//     must be >= 0.
func TestCreateDeploymentOverrides_Validate(t *testing.T) {
	// Free plan env caps: EnvVarsMax=8, EnvValueMaxBytes=4KiB.
	free := MustLimitsFor(Plan("free"))
	// Sanity check — the test pins real plan values.
	if free.EnvVarsMax != 8 || free.EnvValueMaxBytes != 4*1024 {
		t.Fatalf("Free plan limits drifted: EnvVarsMax=%d EnvValueMaxBytes=%d", free.EnvVarsMax, free.EnvValueMaxBytes)
	}

	cases := []struct {
		name       string
		overrides  *CreateDeploymentOverrides
		wantStatus int // 0 = nil problem; otherwise http status to expect
		wantInBody string
	}{
		{
			name:      "nil-overrides-is-ok",
			overrides: nil,
		},
		{
			name: "happy-path-all-fields",
			overrides: &CreateDeploymentOverrides{
				Entrypoint: []string{"/usr/bin/node", "/srv/app.js"},
				Cmd:        []string{"--port", "9090"},
				Env: map[string]string{
					"LOG_LEVEL": "debug",
				},
				EnvSecrets: map[string]string{
					// Secret NAME follows POSIX env-var grammar (uppercase
					// + digits + underscore). ADR-053 §Decision 1 — same
					// shape as env keys / sealed-secret keys, no drift.
					"DB_URL": "secret:DB_URL",
				},
				Port: 9090,
				Healthcheck: &DeploymentHealthcheck{
					Path:      "/healthz",
					IntervalS: 5,
					TimeoutS:  2,
					Retries:   3,
				},
			},
		},
		{
			name: "empty-entrypoint-element",
			overrides: &CreateDeploymentOverrides{
				Entrypoint: []string{"/usr/bin/node", ""},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "entrypoint[1]",
		},
		{
			name: "empty-cmd-element",
			overrides: &CreateDeploymentOverrides{
				Entrypoint: []string{"/usr/bin/node"},
				Cmd:        []string{"--port", ""},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "cmd[1]",
		},
		{
			name: "env-count-exceeds-quota",
			overrides: &CreateDeploymentOverrides{
				// Free caps at 8 env+env_secrets; this case has 9.
				Env: map[string]string{
					"A": "1", "B": "1", "C": "1", "D": "1", "E": "1",
					"F": "1", "G": "1", "H": "1", "I": "1",
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "Override env count exceeded",
		},
		{
			name: "env-shared-cap-includes-env-secrets",
			overrides: &CreateDeploymentOverrides{
				// 5 env + 5 env_secrets = 10 > Free 8; the shared cap
				// catches it even though env alone is under-quota.
				Env: map[string]string{
					"A": "1", "B": "1", "C": "1", "D": "1", "E": "1",
				},
				EnvSecrets: map[string]string{
					"F": "secret:f", "G": "secret:g", "H": "secret:h",
					"I": "secret:i", "J": "secret:j",
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "Override env count exceeded",
		},
		{
			name: "env-key-violates-grammar",
			overrides: &CreateDeploymentOverrides{
				Env: map[string]string{
					"lower_case": "value",
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "Invalid env var key",
		},
		{
			name: "env-value-exceeds-byte-cap",
			overrides: &CreateDeploymentOverrides{
				Env: map[string]string{
					// 4097 bytes — Free caps at 4096.
					"KEY": strings.Repeat("x", 4097),
				},
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantInBody: "Env var value too large",
		},
		{
			name: "env-secrets-ref-missing-prefix",
			overrides: &CreateDeploymentOverrides{
				EnvSecrets: map[string]string{
					"DB_URL": "db-url", // missing "secret:" prefix
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: `must start with "secret:"`,
		},
		{
			name: "env-secrets-ref-name-violates-grammar",
			overrides: &CreateDeploymentOverrides{
				EnvSecrets: map[string]string{
					"DB_URL": "secret:db-url", // lowercase name
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "ref name",
		},
		{
			name: "env-secrets-value-exceeds-byte-cap",
			overrides: &CreateDeploymentOverrides{
				// Ref string length > 4096 (Free cap).
				EnvSecrets: map[string]string{
					"DB_URL": "secret:" + strings.Repeat("X", 4097),
				},
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantInBody: "Env var value too large",
		},
		{
			name: "port-zero-is-absent",
			overrides: &CreateDeploymentOverrides{
				Port: 0, // 0 = absent / fall back to image default
			},
		},
		{
			name: "port-one-is-valid",
			overrides: &CreateDeploymentOverrides{
				Port: 1,
			},
		},
		{
			name: "port-65535-is-valid",
			overrides: &CreateDeploymentOverrides{
				Port: 65535,
			},
		},
		{
			name: "port-70000-is-out-of-range",
			overrides: &CreateDeploymentOverrides{
				Port: 70000,
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "port 70000 out of range",
		},
		{
			name: "port-negative-is-out-of-range",
			overrides: &CreateDeploymentOverrides{
				Port: -1,
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "out of range",
		},
		{
			name: "healthcheck-path-must-start-with-slash",
			overrides: &CreateDeploymentOverrides{
				Healthcheck: &DeploymentHealthcheck{
					Path: "healthz",
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: `must start with "/"`,
		},
		{
			name: "healthcheck-negative-interval",
			overrides: &CreateDeploymentOverrides{
				Healthcheck: &DeploymentHealthcheck{
					Path:      "/healthz",
					IntervalS: -1,
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "interval_s",
		},
		{
			name: "healthcheck-negative-timeout",
			overrides: &CreateDeploymentOverrides{
				Healthcheck: &DeploymentHealthcheck{
					Path:     "/healthz",
					TimeoutS: -1,
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "timeout_s",
		},
		{
			name: "healthcheck-negative-retries",
			overrides: &CreateDeploymentOverrides{
				Healthcheck: &DeploymentHealthcheck{
					Path:    "/healthz",
					Retries: -1,
				},
			},
			wantStatus: http.StatusBadRequest,
			wantInBody: "retries",
		},
		{
			name: "healthcheck-minimal-path-only",
			overrides: &CreateDeploymentOverrides{
				Healthcheck: &DeploymentHealthcheck{
					Path: "/healthz",
					// interval/timeout/retries default to 0 → still valid.
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.overrides.Validate(free)
			if tc.wantStatus == 0 {
				if p != nil {
					t.Fatalf("Validate returned %v (%s), want nil", p, p.Detail)
				}
				return
			}
			if p == nil {
				t.Fatalf("Validate returned nil, want Problem with status=%d and body containing %q", tc.wantStatus, tc.wantInBody)
			}
			if p.Status != tc.wantStatus {
				t.Errorf("Validate status = %d, want %d", p.Status, tc.wantStatus)
			}
			if !strings.Contains(p.Detail, tc.wantInBody) && !strings.Contains(p.Title, tc.wantInBody) {
				t.Errorf("Validate detail = %q, want it to contain %q", p.Detail, tc.wantInBody)
			}
		})
	}
}

// TestCreateDeploymentRequest_AcceptsOverrides pins the request
// shape: Overrides is an optional field; nil/omitted parses cleanly;
// populated parses into the struct. The handler does its own
// Validate(limits) call (handlers.go:159) so this test only pins
// the decode side.
func TestCreateDeploymentRequest_AcceptsOverrides(t *testing.T) {
	t.Run("no-overrides-field", func(t *testing.T) {
		body := []byte(`{"image":"registry.gregale.dev/app@sha256:` + strings.Repeat("a", 64) + `"}`)
		var req CreateDeploymentRequest
		if err := decodeForTest(body, &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Overrides != nil {
			t.Fatalf("Overrides = %+v, want nil", req.Overrides)
		}
	})
	t.Run("with-overrides", func(t *testing.T) {
		body := []byte(`{
			"image":"registry.gregale.dev/app@sha256:` + strings.Repeat("a", 64) + `",
			"overrides":{
				"entrypoint":["/usr/bin/node","/srv/app.js"],
				"cmd":["--port","9090"],
				"env":{"LOG_LEVEL":"debug"},
				"env_secrets":{"DB_URL":"secret:DB_URL"},
				"port":9090,
				"healthcheck":{"path":"/healthz","interval_s":5,"timeout_s":2,"retries":3}
			}
		}`)
		var req CreateDeploymentRequest
		if err := decodeForTest(body, &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Overrides == nil {
			t.Fatal("Overrides is nil; want non-nil")
		}
		if got := req.Overrides.Port; got != 9090 {
			t.Errorf("Overrides.Port = %d, want 9090", got)
		}
		if got := req.Overrides.Env["LOG_LEVEL"]; got != "debug" {
			t.Errorf("Overrides.Env[LOG_LEVEL] = %q, want debug", got)
		}
		if got := req.Overrides.EnvSecrets["DB_URL"]; got != "secret:DB_URL" {
			t.Errorf("Overrides.EnvSecrets[DB_URL] = %q, want secret:DB_URL", got)
		}
		if req.Overrides.Healthcheck == nil || req.Overrides.Healthcheck.Path != "/healthz" {
			t.Errorf("Overrides.Healthcheck = %+v, want path=/healthz", req.Overrides.Healthcheck)
		}
	})
	t.Run("unknown-field-is-rejected", func(t *testing.T) {
		// Issue #460 / ADR-053: the override field list is frozen.
		// DisallowUnknownFields on the handler's decoder means an
		// unknown override field 400s the request — this is the
		// "frozen surface" enforcement on the wire side, complementing
		// the ADR's "no new fields" decision. The handler uses
		// decodeJSON which wires DisallowUnknownFields; this test pins
		// that contract for the override shape.
		body := []byte(`{
			"image":"registry.gregale.dev/app@sha256:` + strings.Repeat("a", 64) + `",
			"overrides":{"volume_mounts":[{"path":"/data","size_mb":1024}]}
		}`)
		var req CreateDeploymentRequest
		err := decodeForTest(body, &req)
		if err == nil {
			t.Fatal("decode succeeded; want error for unknown field volume_mounts")
		}
		if !strings.Contains(err.Error(), "volume_mounts") && !strings.Contains(err.Error(), "unknown") {
			t.Errorf("error = %v, want it to mention volume_mounts (the unknown field)", err)
		}
	})
}
