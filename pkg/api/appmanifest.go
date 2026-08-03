package api

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// AppManifestPath is where imaged writes the manifest inside the app layer and
// where guest-init reads it at boot (spec §4.6, §4.8).
const AppManifestPath = "/etc/faas/app.json"

// Defaults for the guest runtime contract (spec §4.8, §4.9).
const (
	DefaultAppPort = 8080  // the :8080 contract
	DefaultAppUser = "app" // uid 1000 inside the guest
	DefaultAppUID  = 1000
)

// AppManifest is the /etc/faas/app.json contract: the single handoff from the
// build/imaging side (imaged) to the guest side (guest-init). imaged writes it
// into the app layer; guest-init applies env, execs the entrypoint as the app
// user, and uses Port/Healthz for readiness. Keep this struct stable — it is a
// cross-boundary contract baked into every snapshot.
type AppManifest struct {
	// Entrypoint is the exec argv for the customer app. Required.
	Entrypoint []string `json:"entrypoint"`
	// Env is applied before exec. Secret values are injected at boot, not stored
	// here (spec gap G2) — never put secrets in the manifest.
	Env map[string]string `json:"env,omitempty"`
	// EnvSecrets carries sealed-secret REFs ("secret:NAME" strings); the host
	// resolves them at wake against the app_secrets table (issue #460 /
	// ADR-053 §Decision 1). Values NEVER contain plaintext — only refs.
	// guest-init does not read this field; pkg/sched/engine.go's
	// loadSealedEnvFor consumes it via the deployment row, not the manifest.
	EnvSecrets map[string]string `json:"env_secrets,omitempty"`
	// WorkingDir is the app's cwd; empty means "/".
	WorkingDir string `json:"working_dir,omitempty"`
	// Port is the readiness/serving port; 0 means DefaultAppPort.
	Port int `json:"port,omitempty"`
	// Healthz, if set, is a GET path guest-init probes for readiness instead of a
	// bare TCP accept (spec §4.8).
	Healthz string `json:"healthz,omitempty"`
	// User is the unix user to exec as; empty means DefaultAppUser.
	User string `json:"user,omitempty"`
}

// EffectivePort returns Port or the default.
func (m AppManifest) EffectivePort() int {
	if m.Port == 0 {
		return DefaultAppPort
	}
	return m.Port
}

// EffectiveUser returns User or the default.
func (m AppManifest) EffectiveUser() string {
	if m.User == "" {
		return DefaultAppUser
	}
	return m.User
}

// EffectiveWorkingDir returns WorkingDir or "/".
func (m AppManifest) EffectiveWorkingDir() string {
	if m.WorkingDir == "" {
		return "/"
	}
	return m.WorkingDir
}

// Validate rejects a manifest that guest-init could not act on.
func (m AppManifest) Validate() error {
	if len(m.Entrypoint) == 0 {
		return fmt.Errorf("app manifest: empty entrypoint")
	}
	if m.Entrypoint[0] == "" {
		return fmt.Errorf("app manifest: empty entrypoint[0]")
	}
	if m.Port < 0 || m.Port > 65535 {
		return fmt.Errorf("app manifest: port %d out of range", m.Port)
	}
	// EnvSecrets: each value must be a "secret:NAME" ref (ADR-053 §Decision 1).
	// The grammar is shared with pkg/api/dto.go::CreateDeploymentOverrides
	// validation; we duplicate the check here (rather than import) so the
	// manifest contract is self-contained — guest-init and imaged validate
	// without depending on the apid DTO package. The full ref-name regex lives
	// in dto.go for now; if a third caller appears, export it.
	for k, v := range m.EnvSecrets {
		if !strings.HasPrefix(v, SecretRefPrefix) {
			return fmt.Errorf("app manifest: env_secrets[%q]=%q must start with %q", k, v, SecretRefPrefix)
		}
		name := strings.TrimPrefix(v, SecretRefPrefix)
		if !SecretRefNameRe.MatchString(name) {
			return fmt.Errorf("app manifest: env_secrets[%q] ref name %q must match %s", k, name, SecretRefNameRe.String())
		}
	}
	return nil
}

// WriteManifest encodes m as canonical JSON.
func WriteManifest(w io.Writer, m AppManifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// ReadManifest decodes and validates a manifest (guest-init's boot path).
func ReadManifest(r io.Reader) (AppManifest, error) {
	var m AppManifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return AppManifest{}, fmt.Errorf("app manifest: decode: %w", err)
	}
	if err := m.Validate(); err != nil {
		return AppManifest{}, err
	}
	return m, nil
}

// SidecarBuildManifest returns a placeholder AppManifest imaged
// bakes into a sidecar's drive1 (issue #463 / ADR-069 / PR-B).
//
// The placeholder exists because pkg/api.AppManifest.Validate
// rejects an empty entrypoint, and rootfs.Builder.Build calls
// Validate on its way through. Sidecars do not have a customer
// entrypoint — guest-init reads the per-workload workload.json
// (one per drive, written by vmmd at boot per
// pkg/fcvm/vmm.go::StageSecretsEnv generalization) to discover
// argv/env/port for each sidecar at runtime. The placeholder is
// therefore never executed: guest-init's per-workload supervisor
// execs the sidecar argv from workload.json, not the rootfs-
// baked app.json. The string "/bin/sidecar-placeholder" is a
// stable marker an operator can grep for if the placeholder
// ever surfaces in a crash log (it should not — guest-init
// reads workload.json exclusively for sidecars).
func SidecarBuildManifest() AppManifest {
	return AppManifest{
		Entrypoint: []string{"/bin/sidecar-placeholder"},
		Port:       DefaultAppPort,
		Healthz:    "/healthz",
	}
}
