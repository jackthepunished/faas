// Package fleetbundle defines the signed, provider-neutral authorization
// document for adopting already-created compute nodes.
//
// A FleetEnrollmentBundle is deliberately separate from an application
// release. It carries short-lived provider connection facts and host-key
// attestations; the signed release and production manifest remain the source
// of truth for code, roles, topology, PKI, and runtime endpoints.
package fleetbundle

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/nodeclaim"
	"gopkg.in/yaml.v3"
)

const (
	Kind = "FleetEnrollmentBundle"

	// MaxLifetime keeps a leaked provider address and host-key authorization
	// useful for only a bounded onboarding window.
	MaxLifetime = 7 * 24 * time.Hour
	// ClockSkew tolerates small differences between the signing and runner
	// clocks without accepting a materially stale bundle.
	ClockSkew = 5 * time.Minute
	// NonceBytes is the minimum entropy required for a replay identity.
	NonceBytes = 16
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)
	// Raw URL encoding keeps the nonce safe in filenames and GitHub outputs.
	noncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

// Bundle is the signed fleet-enrollment authorization document.
type Bundle struct {
	APIVersion string   `yaml:"api_version" json:"api_version"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name       string `yaml:"name" json:"name"`
	Generation uint64 `yaml:"generation" json:"generation"`
}

type Spec struct {
	IssuedAt  time.Time         `yaml:"issued_at" json:"issued_at"`
	ExpiresAt time.Time         `yaml:"expires_at" json:"expires_at"`
	Nonce     string            `yaml:"nonce" json:"nonce"`
	Claims    []nodeclaim.Claim `yaml:"claims" json:"claims"`
}

// New creates an unsigned enrollment document from provider-produced claims.
// The caller must sign the returned bytes through the trusted publisher before
// a join can use them. A fresh nonce makes every authorization generation
// distinct even when it names the same node.
func New(name string, generation uint64, now time.Time, lifetime time.Duration, claims []nodeclaim.Claim) (Bundle, error) {
	if lifetime <= 0 || lifetime > MaxLifetime {
		return Bundle{}, fmt.Errorf("fleetbundle: lifetime must be positive and no longer than %s", MaxLifetime)
	}
	nonceBytes := make([]byte, NonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return Bundle{}, fmt.Errorf("fleetbundle: generate nonce: %w", err)
	}
	bundle := Bundle{
		APIVersion: nodeclaim.APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: name, Generation: generation},
		Spec: Spec{
			IssuedAt:  now.UTC(),
			ExpiresAt: now.UTC().Add(lifetime),
			Nonce:     base64.RawURLEncoding.EncodeToString(nonceBytes),
			Claims:    append([]nodeclaim.Claim(nil), claims...),
		},
	}
	if errs := bundle.Validate(); errs != nil {
		return Bundle{}, errs
	}
	return bundle, nil
}

// Marshal returns the exact YAML bytes to hand to the signing tool. It does
// not add a signature or normalize after signing; callers must sign these
// returned bytes and later verify the same file without rewriting it.
func Marshal(bundle Bundle) ([]byte, error) {
	if errs := bundle.Validate(); errs != nil {
		return nil, errs
	}
	return yaml.Marshal(bundle)
}

// Error is one deterministic bundle validation failure.
type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e Error) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Message) }

type Errors []Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

func (e Errors) Is(target error) bool { return target == ErrInvalid }

var (
	ErrInvalid       = errors.New("fleetbundle: invalid")
	ErrExpired       = errors.New("fleetbundle: expired")
	ErrReplay        = errors.New("fleetbundle: enrollment already consumed")
	ErrStateRequired = errors.New("fleetbundle: replay state directory is required")
)

// Load reads and parses one bundle document. Signature verification is a
// separate operation and must happen before trusting the parsed values.
func Load(path string) (*Bundle, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fleetbundle: read %s: %w", path, err)
	}
	return Parse(body)
}

// Parse accepts YAML or JSON and rejects unknown fields and multiple YAML
// documents. The exact original bytes remain the object covered by the
// detached signature.
func Parse(body []byte) (*Bundle, error) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	var bundle Bundle
	if err := dec.Decode(&bundle); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("fleetbundle: empty file")
		}
		return nil, fmt.Errorf("fleetbundle: parse: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("fleetbundle: multiple documents are not allowed")
		}
		return nil, fmt.Errorf("fleetbundle: parse trailing document: %w", err)
	}
	return &bundle, nil
}

// Validate checks the structural and authorization invariants that do not
// depend on the current clock or a production manifest.
func (b Bundle) Validate() Errors {
	var errs Errors
	if b.APIVersion != nodeclaim.APIVersion {
		errs = append(errs, Error{"api_version", fmt.Sprintf("must be %q", nodeclaim.APIVersion)})
	}
	if b.Kind != Kind {
		errs = append(errs, Error{"kind", fmt.Sprintf("must be %q", Kind)})
	}
	if b.Metadata.Name == "" {
		errs = append(errs, Error{"metadata.name", "is required"})
	} else if !namePattern.MatchString(b.Metadata.Name) {
		errs = append(errs, Error{"metadata.name", "must contain only lowercase letters, digits, dots, or dashes"})
	}
	if b.Metadata.Generation == 0 {
		errs = append(errs, Error{"metadata.generation", "must be greater than zero"})
	}
	if b.Spec.IssuedAt.IsZero() {
		errs = append(errs, Error{"spec.issued_at", "is required"})
	}
	if b.Spec.ExpiresAt.IsZero() {
		errs = append(errs, Error{"spec.expires_at", "is required"})
	}
	if !b.Spec.IssuedAt.IsZero() && !b.Spec.ExpiresAt.IsZero() {
		if !b.Spec.ExpiresAt.After(b.Spec.IssuedAt) {
			errs = append(errs, Error{"spec.expires_at", "must be after spec.issued_at"})
		} else if b.Spec.ExpiresAt.Sub(b.Spec.IssuedAt) > MaxLifetime {
			errs = append(errs, Error{"spec.expires_at", fmt.Sprintf("lifetime must not exceed %s", MaxLifetime)})
		}
	}
	if !noncePattern.MatchString(b.Spec.Nonce) {
		errs = append(errs, Error{"spec.nonce", "must be 22-character raw URL-base64 (at least 128 bits)"})
	} else if decoded, err := base64.RawURLEncoding.DecodeString(b.Spec.Nonce); err != nil || len(decoded) != NonceBytes {
		errs = append(errs, Error{"spec.nonce", "must decode to exactly 16 bytes"})
	}
	if len(b.Spec.Claims) == 0 {
		errs = append(errs, Error{"spec.claims", "must contain at least one claim"})
	}
	seen := make(map[string]struct{}, len(b.Spec.Claims))
	for i, claim := range b.Spec.Claims {
		path := fmt.Sprintf("spec.claims[%d]", i)
		if claimErrs := claim.Validate(); claimErrs != nil {
			for _, claimErr := range claimErrs {
				errs = append(errs, Error{path + "." + claimErr.Path, claimErr.Message})
			}
			continue
		}
		if _, ok := seen[claim.Metadata.Name]; ok {
			errs = append(errs, Error{path + ".metadata.name", fmt.Sprintf("duplicate node %q", claim.Metadata.Name)})
			continue
		}
		seen[claim.Metadata.Name] = struct{}{}
		if claim.Spec.SSH.HostKeySHA256 == "" {
			errs = append(errs, Error{path + ".spec.ssh.host_key_sha256", "is required in a signed fleet-enrollment bundle"})
		}
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// ValidateAt applies structural checks plus the short-lived authorization
// window. A bundle may be used slightly before/after its timestamps only
// within ClockSkew; it cannot be used after expiry in normal operation.
func (b Bundle) ValidateAt(now time.Time) Errors {
	errs := b.Validate()
	if len(errs) != 0 || b.Spec.IssuedAt.IsZero() || b.Spec.ExpiresAt.IsZero() {
		return errs
	}
	now = now.UTC()
	if b.Spec.IssuedAt.After(now.Add(ClockSkew)) {
		errs = append(errs, Error{"spec.issued_at", "is too far in the future"})
	}
	if b.Spec.ExpiresAt.Before(now.Add(-ClockSkew)) {
		errs = append(errs, Error{"spec.expires_at", "has expired"})
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

// ClaimForNode returns the one normalized claim authorized for node.
func (b Bundle) ClaimForNode(node string) (nodeclaim.Node, error) {
	if errs := b.Validate(); errs != nil {
		return nodeclaim.Node{}, errs
	}
	for _, claim := range b.Spec.Claims {
		if claim.Metadata.Name == node {
			return claim.Normalize(), nil
		}
	}
	return nodeclaim.Node{}, fmt.Errorf("fleetbundle: bundle %q does not authorize node %q", b.Metadata.Name, node)
}

// Digest returns the sha256 digest of the exact signed bytes.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ReplayKey identifies one authorized node enrollment. A new generation or
// nonce produces a new key, while retrying the same signed authorization is
// rejected after a successful join.
func ReplayKey(b Bundle, node string) string {
	value := fmt.Sprintf("%s\x00%d\x00%s\x00%s", b.Metadata.Name, b.Metadata.Generation, b.Spec.Nonce, node)
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ReplayState is a durable, atomic single-use ledger. The join command checks
// it before remote work and marks it only after the node has been activated;
// a failed join can therefore be resumed without consuming its authorization.
type ReplayState struct {
	Dir string
}

func (s ReplayState) Check(b Bundle, node string) error {
	if s.Dir == "" {
		return ErrStateRequired
	}
	_, err := os.Stat(s.markerPath(b, node))
	if err == nil {
		return ErrReplay
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("fleetbundle: check replay state: %w", err)
}

func (s ReplayState) Mark(b Bundle, node string) error {
	if s.Dir == "" {
		return ErrStateRequired
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("fleetbundle: create replay state: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("fleetbundle: secure replay state: %w", err)
	}
	path := s.markerPath(b, node)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrReplay
		}
		return fmt.Errorf("fleetbundle: create replay marker: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%s\n", ReplayKey(b, node)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("fleetbundle: write replay marker: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("fleetbundle: sync replay marker: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("fleetbundle: close replay marker: %w", err)
	}
	return nil
}

func (s ReplayState) markerPath(b Bundle, node string) string {
	return filepath.Join(s.Dir, ReplayKey(b, node)+".used")
}
