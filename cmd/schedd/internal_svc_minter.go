package main

// internal_svc_minter.go — ADR-119 minting surface for the
// outbound Authorization: Bearer JWT schedd attaches to
// /v1/synthesize requests targeting apps whose
// public_auth_mode='internal_only' (issue #477 #4).
//
// Production keypair loading: the operator provisions the
// Ed25519 keypair at /etc/faas/secrets/internal-svc/schedd.ed25519
// (override via FAAS_INTERNAL_SVC_KEY_PATH). The key is
// generated fresh on first boot if missing — a loud WARN log
// makes this visible so the operator can persist it to
// host.age. The corresponding public key is added to the
// FAAS_INTERNAL_SVC_PUBKEYS env on every gatewayd-internal
// node. Rotation: out of scope for PR-A (ADR-120 candidate).

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/onebox-faas/faas/pkg/internalsvc"
)

const (
	// internalSvcTokenTTL is the JWT exp claim window (ADR-119
	// plan: short TTL for replay-attack posture, ≤30s). Chosen
	// at 30s to match a typical cron-fire latency budget
	// (gatewayd-internal wake + first byte ≤25s in p99).
	internalSvcTokenTTL = 30 * time.Second
	// internalSvcKeyPathEnv is the env var that overrides the
	// default keypair path.
	internalSvcKeyPathEnv = "FAAS_INTERNAL_SVC_KEY_PATH"
	// defaultInternalSvcKeyPath is the production path the
	// operator is expected to provision. Used when the env is
	// unset.
	defaultInternalSvcKeyPath = "/etc/faas/secrets/internal-svc/schedd.ed25519"
)

// newSchedInternalSvcMinter loads the schedd Ed25519 keypair
// from FAAS_INTERNAL_SVC_KEY_PATH (or the default path), or
// generates a fresh keypair with a loud WARN if the file is
// missing. Returns a closure of type
// func(appID string) (string, error) — the signature
// pkg/sched.loop.go::httpGatewaySynth.mintInternalSvcToken
// expects.
func newSchedInternalSvcMinter(log *slog.Logger) (func(appID string) (string, error), error) {
	if log == nil {
		log = slog.Default()
	}
	keyPath := os.Getenv(internalSvcKeyPathEnv)
	if keyPath == "" {
		keyPath = defaultInternalSvcKeyPath
	}
	priv, err := loadOrGenerateSchedKey(keyPath, log)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	kid := kidFromPub(pub)
	log.Info("schedd: internal-svc minter loaded",
		"svc_name", "schedd",
		"kid", kid,
		"key_path", keyPath,
		"ttl", internalSvcTokenTTL.String())
	return func(appID string) (string, error) {
		claims := map[string]any{
			// Future: per-app key-pinning — the receiver
			// could refuse tokens whose app_id claim doesn't
			// match the routed app. Today's receiver just
			// checks svcName + aud + exp + sig, so we include
			// app_id for audit-log fidelity only.
			"app_id": appID,
		}
		return internalsvc.Mint("schedd", internalSvcTokenTTL, claims, priv, kid)
	}, nil
}

// loadOrGenerateSchedKey loads the Ed25519 private key from
// the given PEM file path, or generates a fresh keypair and
// persists it if the file is missing. The PEM shape is the
// PKCS#8 PrivateKey wrapped in "PRIVATE KEY".
func loadOrGenerateSchedKey(keyPath string, log *slog.Logger) (ed25519.PrivateKey, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("schedd: %s is not PEM-encoded", keyPath)
		}
		if block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("schedd: %s has unexpected PEM block type %q", keyPath, block.Type)
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("schedd: parse %s: %w", keyPath, err)
		}
		priv, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("schedd: %s is not an Ed25519 key", keyPath)
		}
		return priv, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("schedd: read %s: %w", keyPath, err)
	}
	// File missing — generate a fresh keypair and persist.
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, fmt.Errorf("schedd: generate keypair: %w", err)
	}
	if dir := filepath.Dir(keyPath); dir != "" {
		if mkErr := os.MkdirAll(dir, 0700); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			log.Warn("schedd: mkdir for internal-svc key failed; minter will not persist",
				"path", dir, "err", mkErr.Error())
			return priv, nil
		}
	}
	marshalled, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		log.Warn("schedd: marshal internal-svc key failed; minter will not persist",
			"err", err.Error())
		return priv, nil
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: marshalled})
	if wErr := os.WriteFile(keyPath, pemBytes, 0600); wErr != nil {
		log.Warn("schedd: persist internal-svc key failed; minter will be in-memory only",
			"path", keyPath, "err", wErr.Error())
		return priv, nil
	}
	log.Warn("schedd: generated a fresh internal-svc keypair — move it into host.age before the next deploy",
		"path", keyPath)
	return priv, nil
}

// kidFromPub returns the base64url-encoded sha256 of the
// first 16 bytes of the public key. Truncated for header
// compactness; collision risk at 16 bytes is negligible for
// the ≤10 services expected in production.
func kidFromPub(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}