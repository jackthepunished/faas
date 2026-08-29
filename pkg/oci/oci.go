package oci

import (
	"encoding/json"
	"fmt"
	"io"
)

// Shared OCI image-config raw decoder.
//
// The OCI image-config spec
// (https://github.com/opencontainers/image-spec/blob/main/config.md)
// allows fields at either the top level ("Docker v2 flat") or inside a
// nested "config" envelope ("OCI nested"); in practice some registries
// emit one, some emit the other, and a few emit BOTH. The package's
// two parsers (ParseConfig, parseImageConfig) historically read
// different subsets of fields and preferred the formats differently,
// resulting in drift: the registry path silently dropped Entrypoint and
// User, and the two paths disagreed on Cmd when both envelopes were
// present.
//
// ADR-136 (issue #1186, M-1 commit 3) makes this file the single raw
// decoder. Both callers unmarshal into rawConfig and call resolved()
// for the flat-or-nested preference; new OCI fields get one parser,
// not two.
//
// Reference: ADR-040 (layer symlink policy) is unaffected; ADR-053
// (deploy overrides) operates at a higher layer.

// rawConfig is the unmarshalled-once view of an OCI/Docker
// image-config blob. Pointer-to-nested lets us distinguish "absent"
// from "present-but-empty" for the OCI `config` envelope.
type rawConfig struct {
	// Flat fields (Docker v2 schema).
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	Entrypoint []string `json:"Entrypoint"`
	User       string   `json:"User"`

	// Nested `config` envelope (OCI image-config). Optional — many
	// registry implementations omit it entirely.
	Config *rawNestedConfig `json:"config"`

	// rootfs is the OCI-spec struct (always present in OCI images;
	// absent in pure Docker v2 manifests; tolerate both).
	RootFS struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// rawNestedConfig is the inner envelope of an OCI image-config.
type rawNestedConfig struct {
	Cmd          []string            `json:"Cmd"`
	Env          []string            `json:"Env"`
	WorkingDir   string              `json:"WorkingDir"`
	Entrypoint   []string            `json:"Entrypoint"`
	User         string              `json:"User"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
}

// rawFields is the resolved single-source-of-truth view: each field is
// the flat value if non-empty, otherwise the nested-`config` value if
// present, otherwise zero.
type rawFields struct {
	Cmd        []string
	Env        []string
	WorkingDir string
	Entrypoint []string
	User       string
}

// resolved applies the flat-then-nested precedence rule. Preserves the
// historical preference (flat wins) so today's deployments don't
// change shape; ADR-136 §Decision 1 records the rationale.
func (r *rawConfig) resolved() rawFields {
	f := rawFields{
		Cmd:        r.Cmd,
		Env:        r.Env,
		WorkingDir: r.WorkingDir,
		Entrypoint: r.Entrypoint,
		User:       r.User,
	}
	if r.Config != nil {
		if len(f.Cmd) == 0 {
			f.Cmd = r.Config.Cmd
		}
		if len(f.Env) == 0 {
			f.Env = r.Config.Env
		}
		if f.WorkingDir == "" {
			f.WorkingDir = r.Config.WorkingDir
		}
		if len(f.Entrypoint) == 0 {
			f.Entrypoint = r.Config.Entrypoint
		}
		if f.User == "" {
			f.User = r.Config.User
		}
	}
	return f
}

// validate returns an error if the rootfs.type is set to anything other
// than "layers" (the only mode the platform supports today).
func (r *rawConfig) validate() error {
	if r.RootFS.Type != "" && r.RootFS.Type != "layers" {
		return fmt.Errorf("oci: unsupported rootfs type %q", r.RootFS.Type)
	}
	return nil
}

// decodeRaw is the single unmarshal site for both ParseConfig and
// parseImageConfig. Callers then project resolved() onto their own
// consumer-facing struct (oci.Config or oci.ImageConfig).
func decodeRaw(r io.Reader) (*rawConfig, error) {
	var raw rawConfig
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, fmt.Errorf("oci: parse config: %w", err)
	}
	return &raw, nil
}
