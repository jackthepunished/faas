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
//
// Sealed-at-rest posture (CLAUDE.md G2 §17, round-3 peer-review
// finding #5): the operator MAY instead provision
// FAAS_INTERNAL_SVC_KEY_SEALED_BLOB (the age-encrypted PEM bytes,
// produced by `hostage-gen seal --namespace internal_svc`) and
// FAAS_INTERNAL_SVC_KEY_SEALED_NAMESPACE (default
// "internal_svc"). When the sealed env is present, schedd
// unseals via secretbox.OpenBytesMulti against the host.age
// identities (current + previous) and never touches plaintext
// PEM on disk. The plaintext path stays for local dev + the
// legacy operator who hasn't migrated yet — both paths run
// the same key-shape validation.

import (
	"crypto/ed25519"
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
	"github.com/onebox-faas/faas/pkg/secretbox"
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
	// internalSvcKeySealedEnv holds the age-encrypted PEM bytes
	// (output of `hostage-gen seal --namespace internal_svc`).
	// Round-3 G2 §17 closure: secrets at rest are sealed via
	// host.age. Operators set this in the systemd unit
	// EnvironmentFile; schedd unseals via host.age on boot.
	internalSvcKeySealedEnv = "FAAS_INTERNAL_SVC_KEY_SEALED_BLOB"
	// internalSvcKeySealedNamespaceEnv overrides the seal
	// namespace; defaults to "internal_svc" when unset.
	internalSvcKeySealedNamespaceEnv = "FAAS_INTERNAL_SVC_KEY_SEALED_NAMESPACE"
	// internalSvcKeySealedNamespaceDefault matches the
	// reservation in ADR-119 §Deployment requirements
	// ("namespace 'internal_svc' under host.age").
	internalSvcKeySealedNamespaceDefault = "internal_svc"
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
//
// Sealed-at-rest mode: if FAAS_INTERNAL_SVC_KEY_SEALED_BLOB is
// set, the plaintext-PEM path is skipped entirely and the
// unsealed bytes are used. The host.age identities are loaded
// via secretbox.LoadHostKeys(secretbox.DefaultHostKeyDir) —
// current first, previous second — so a rotation overlap window
// is supported without daemon restart.
func newSchedInternalSvcMinter(log *slog.Logger) (func(appID string) (string, error), error) {
	if log == nil {
		log = slog.Default()
	}
	priv, source, err := loadSchedInternalSvcKey(log)
	if err != nil {
		return nil, err
	}
	pub := priv.Public().(ed25519.PublicKey)
	kid := kidFromPub(pub)
	log.Info("schedd: internal-svc minter loaded",
		"svc_name", "schedd",
		"kid", kid,
		"source", source,
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

// loadSchedInternalSvcKey is the new top-level loader (round-3
// G2 §17 closure). It picks the sealed-at-rest path when
// FAAS_INTERNAL_SVC_KEY_SEALED_BLOB is set, falling back to the
// plaintext-PEM path otherwise. Returns the private key + a
// short source tag for the boot log ("sealed" vs
// "plaintext_pem:<path>").
func loadSchedInternalSvcKey(log *slog.Logger) (ed25519.PrivateKey, string, error) {
	if log == nil {
		// Same default as newSchedInternalSvcMinter — nil-safe
		// for tests that bypass the public entry point.
		log = slog.Default()
	}
	if sealed := os.Getenv(internalSvcKeySealedEnv); sealed != "" {
		priv, err := loadSchedKeySealed(sealed, log)
		if err != nil {
			return nil, "", err
		}
		return priv, "sealed", nil
	}
	keyPath := os.Getenv(internalSvcKeyPathEnv)
	if keyPath == "" {
		keyPath = defaultInternalSvcKeyPath
	}
	priv, err := loadOrGenerateSchedKey(keyPath, log)
	if err != nil {
		return nil, "", err
	}
	return priv, "plaintext_pem:" + keyPath, nil
}

// loadSchedKeySealed unseals the PEM bytes from
// FAAS_INTERNAL_SVC_KEY_SEALED_BLOB against the host.age
// identities (current + previous). Round-3 G2 §17 closure —
// the key never sits in plaintext on disk. The seal namespace
// is taken from FAAS_INTERNAL_SVC_KEY_SEALED_NAMESPACE
// (default "internal_svc"); the namespace is checked on open
// so a stolen ciphertext from a different namespace cannot be
// replayed against the internal-svc path.
func loadSchedKeySealed(sealedB64 string, log *slog.Logger) (ed25519.PrivateKey, error) {
	identities, err := secretbox.LoadHostKeys(filepath.Dir(secretbox.DefaultHostKeyPath))
	if err != nil {
		return nil, fmt.Errorf("schedd: load host.age identities for sealed key: %w", err)
	}
	if len(identities) == 0 {
		return nil, errors.New("schedd: no host.age identities available; cannot unseal FAAS_INTERNAL_SVC_KEY_SEALED_BLOB")
	}
	ns := os.Getenv(internalSvcKeySealedNamespaceEnv)
	if ns == "" {
		ns = internalSvcKeySealedNamespaceDefault
	}
	// Decode the base64 wrapper around the age ciphertext
	// (matches the on-wire shape produced by `hostage-gen
	// seal`, which base64-encodes the age binary output so
	// it lands cleanly in EnvironmentFile / systemd
	// credentials). If the operator pastes raw age output
	// instead, fall through to OpenBytesMulti on the raw
	// bytes — both shapes are accepted to keep migration
	// friction low.
	var sealed []byte
	if raw, decErr := base64.StdEncoding.DecodeString(sealedB64); decErr == nil && looksLikeAgeBlob(raw) {
		sealed = raw
	} else {
		sealed = []byte(sealedB64)
	}
	gotNS, plaintext, err := secretbox.OpenBytesMulti(identities, sealed)
	if err != nil {
		return nil, fmt.Errorf("schedd: unseal FAAS_INTERNAL_SVC_KEY_SEALED_BLOB: %w", err)
	}
	if gotNS != ns {
		return nil, fmt.Errorf("schedd: sealed key namespace=%q, want %q (refusing cross-namespace replay)", gotNS, ns)
	}
	priv, err := parseSchedKeyPEM(plaintext)
	if err != nil {
		return nil, fmt.Errorf("schedd: parse unsealed PEM: %w", err)
	}
	log.Info("schedd: unsealed internal-svc key from host.age",
		"namespace", gotNS, "identities", len(identities))
	return priv, nil
}

// looksLikeAgeBlob is a cheap heuristic: age output starts
// with the ASCII armor header "-----BEGIN AGE ENCRYPTED FILE-----".
// Used to decide whether the env value is base64-wrapped or
// raw. False positives are harmless (OpenBytesMulti fails
// loudly); false negatives fall through to the raw-bytes path
// and OpenBytesMulti there.
func looksLikeAgeBlob(b []byte) bool {
	const prefix = "-----BEGIN AGE ENCRYPTED FILE-----"
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

// parseSchedKeyPEM decodes the unsealed bytes as the same
// PEM/PKCS#8 Ed25519 shape loadOrGenerateSchedKey writes.
// Lifted so the sealed path produces the same validated
// ed25519.PrivateKey regardless of where the bytes came from.
func parseSchedKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("schedd: sealed payload is not PEM-encoded")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("schedd: sealed payload has unexpected PEM block type %q", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("schedd: parse PKCS#8: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("schedd: sealed payload is not an Ed25519 key")
	}
	return priv, nil
}

// loadOrGenerateSchedKey loads the Ed25519 private key from
// the given PEM file path, or generates a fresh keypair and
// persists it if the file is missing. The PEM shape is the
// PKCS#8 PrivateKey wrapped in "PRIVATE KEY".
//
// Round-3 follow-up note: this plaintext-PEM path is the
// dev-friendly default. Operators are nudged toward the sealed
// path (FAAS_INTERNAL_SVC_KEY_SEALED_BLOB) — the WARN emitted
// when a fresh keypair is generated now also includes the
// `hostage-gen seal --namespace internal_svc` recipe so the
// migration is one command away.
func loadOrGenerateSchedKey(keyPath string, log *slog.Logger) (ed25519.PrivateKey, error) {
	if data, err := os.ReadFile(keyPath); err == nil {
		return parseSchedKeyPEM(data)
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
	log.Warn("schedd: generated a fresh internal-svc keypair — seal it via 'hostage-gen seal --namespace internal_svc < "+keyPath+"' and set FAAS_INTERNAL_SVC_KEY_SEALED_BLOB",
		"path", keyPath)
	return priv, nil
}

// kidFromPub delegates to internalsvc.KidFromPub — the
// canonical kid derivation. Round-3 peer-review #7 (kid
// format divergence): this used to be a local helper that
// produced base64-of-[:16] while pkg/internalsvc's auto-derive
// produced hex-of-[:8]. The drift made diagnostic logs that
// key off kid unreliable. Now both surfaces call
// internalsvc.KidFromPub — a single source of truth. The
// local kidFromPub is kept as a thin wrapper so the boot log
// line and any future code path don't have to import the
// internalsvc package-level function explicitly.
func kidFromPub(pub ed25519.PublicKey) string {
	return internalsvc.KidFromPub(pub)
}
