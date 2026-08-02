package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/secretbox"
)

// setSidecarRecipient is the host X25519 recipient apid loads once
// at startup. Mirrors the `setSecretRecipient` pattern in
// handlers_secrets.go:41 — the recipient is the SAME host age
// key (the host's age identity loads once at boot and is shared
// across the secret / sidecar / registry-credential seal sites).
// Keeping a dedicated getter for the sidecar call site means
// the seal helper is testable in isolation (tests can stub the
// getter to return a generated test recipient without touching
// the secret-handler getter).
//
// Setting this is the responsibility of cmd/apid/main.go's run
// path. The nil-default makes tests that don't seal pass without
// plumbing; a production apid that forgets to load the recipient
// surfaces a clear 503 from every deploy that carries sidecars
// (no silent accept-and-drop).
var setSidecarRecipient func() *age.X25519Recipient

// sealSidecars is the apid-side envelope-seal helper for sidecar
// env values (issue #463 / ADR-066 §Decision 3). It is the load-
// bearing gateway between the wire shape (plaintext env per sidecar)
// and the persisted shape (envelope-sealed ciphertext per env key).
//
// Wire: each sidecar carries a plaintext `env` map.
// Persist: each env VALUE is replaced with a base64-encoded
//
//	`secretbox.SealBytes(recipient, "sidecar_env", plaintext,
//	maxValueBytes)` blob. The KEY stays in plaintext (the
//	env-var name). The audit / log path NEVER sees the
//	plaintext.
//
// The helper returns a `*api.Problem` (not a typed error) so the
// caller's `api.WriteProblem` path stays branch-free:
//   - recipient == nil  → 503 ErrCapacity (recipient not loaded)
//   - per-value seal    → 413 ErrSecretValueTooLarge (the seal
//     itself enforces the per-value byte cap; the API gate also
//     runs before this helper, so the 413 is defence in depth)
//
// The function returns `[]byte("[]")` (NOT nil) when `ss` is empty
// so the persistence path's `notNullEmptyJSONRaw` helper
// (pkg/state/pgstore.go) never sees a nil JSON shape. The
// `deployments.sidecars` column is `NOT NULL DEFAULT '[]'::jsonb`;
// a nil insert would 23502-fail. The empty-input branch is the
// most common path (the no-sidecar deploy case) and it's
// deliberately as cheap as possible.
func sealSidecars(ss api.Sidecars, recipient *age.X25519Recipient) ([]byte, *api.Problem) {
	if recipient == nil {
		return nil, api.ErrCapacity("host age recipient not loaded — refusing to seal sidecar env")
	}
	if len(ss) == 0 {
		return []byte("[]"), nil
	}
	type sealedSidecar struct {
		Name      string            `json:"name"`
		Image     string            `json:"image"`
		Type      api.SidecarType   `json:"type"`
		Cmd       []string          `json:"cmd,omitempty"`
		Env       map[string]string `json:"env,omitempty"`
		Port      int               `json:"port,omitempty"`
		RamMB     int               `json:"ram_mb,omitempty"`
		Essential *bool             `json:"essential,omitempty"`
	}
	out := make([]sealedSidecar, 0, len(ss))
	for _, s := range ss {
		envOut := make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			// Per-value byte cap: the API gate already enforces
			// this via Sidecar.Validate(limits). Passing 0 here
			// disables the seal-time cap (a malformed env value
			// that snuck past the gate would then land in
			// storage; the cap is the only defence). PR-A keeps
			// the per-value cap at the API layer; the seal
			// helper trusts the validated input.
			ct, err := secretbox.SealBytes(recipient, "sidecar_env",
				[]byte(v), 0)
			if err != nil {
				return nil, api.NewProblem(http.StatusInternalServerError,
					api.CodeCapacity, "Failed to seal sidecar env",
					fmt.Sprintf("sidecar %q env[%q] seal: %v", s.Name, k, err))
			}
			envOut[k] = base64.StdEncoding.EncodeToString(ct)
		}
		out = append(out, sealedSidecar{
			Name:      s.Name,
			Image:     s.Image,
			Type:      s.Type,
			Cmd:       s.Cmd,
			Env:       envOut,
			Port:      s.Port,
			RamMB:     s.RamMB,
			Essential: s.Essential,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, api.NewProblem(http.StatusInternalServerError,
			api.CodeCapacity, "Failed to marshal sealed sidecars",
			fmt.Sprintf("marshal sealed sidecars: %v", err))
	}
	return raw, nil
}
