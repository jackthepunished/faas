// seal.go — Seal / Open round-trip for customer secret envelopes.
//
// The on-disk ciphertext is age's standard format (X25519 stanza + ChaCha20-
// Poly1305 body). The plaintext is a canonical-JSON encoding of Envelope so
// Open validates shape before returning to callers.
//
// Two callers exist:
//   - apid: Seal(env) → ciphertext stored in app_secrets.ciphertext.
//   - vmmd: Open(ciphertext) → env (decoded Envelope), loopback-mounted to
//     drive1 as /etc/faas/secrets.env.
//
// guest-init does NOT call secretbox.Open. It reads the cleartext
// /etc/faas/secrets.env (decoded by vmmd at provision time). The seal
// boundary is apid → vmmd, not vmmd → guest.
package secretbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
)

// Envelope is the plaintext shape sealed at rest. JSON-marshalled. Map
// iteration order is non-deterministic; canonical encoding comes from
// json.Marshal which sorts map keys.
type Envelope map[string]string

// Validate checks the envelope shape: every key must match the env-var
// pattern enforced by the app_secrets.key CHECK constraint. Values are
// accepted as-is — byte-length enforcement happens upstream in apid against
// Limits.SecretValueMaxBytes so over-cap values never reach the seal path.
func (e Envelope) Validate() error {
	keyRe := regexp.MustCompile(api.SecretKeyPattern)
	for k := range e {
		if len(k) == 0 || len(k) > api.MaxSecretKeyLen {
			return fmt.Errorf("secretbox: key length %d out of range (0,%d]", len(k), api.MaxSecretKeyLen)
		}
		if !keyRe.MatchString(k) {
			return fmt.Errorf("secretbox: key %q does not match %s", k, api.SecretKeyPattern)
		}
	}
	return nil
}

// Seal encrypts env under recipient using age X25519 + ChaCha20-Poly1305.
// The returned blob is the age file format (ASCII-armoured stanza header
// followed by binary body) suitable for bytea storage in PG.
func Seal(recipient *age.X25519Recipient, env Envelope) ([]byte, error) {
	if recipient == nil {
		return nil, errors.New("secretbox: nil recipient")
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("secretbox: marshal envelope: %w", err)
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, fmt.Errorf("secretbox: open age writer: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("secretbox: write plaintext: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secretbox: close age writer: %w", err)
	}
	return out.Bytes(), nil
}

// Open decrypts blob under identity and returns the decoded Envelope.
// Returns an error if the blob is tampered, the identity doesn't match,
// or the plaintext isn't a valid Envelope.
func Open(identity *age.X25519Identity, blob []byte) (Envelope, error) {
	if identity == nil {
		return nil, errors.New("secretbox: nil identity")
	}
	if len(blob) == 0 {
		return nil, errors.New("secretbox: empty blob")
	}
	r, err := age.Decrypt(bytes.NewReader(blob), identity)
	if err != nil {
		return nil, fmt.Errorf("secretbox: open age reader: %w", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("secretbox: read plaintext: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("secretbox: unmarshal envelope: %w", err)
	}
	if err := env.Validate(); err != nil {
		return nil, err
	}
	return env, nil
}

// SealOne seals a single (key, value) pair. Convenience wrapper for apid's
// PUT handler: it builds the one-entry Envelope, validates SecretValueMaxBytes
// against len(value), and returns the ciphertext blob.
//
// The byte cap is checked HERE (not by the caller) so the seal path is the
// single trust boundary for "no over-cap ciphertext ever lands in PG".
func SealOne(recipient *age.X25519Recipient, key, value string, maxValueBytes int) ([]byte, error) {
	if maxValueBytes > 0 && len(value) > maxValueBytes {
		return nil, api.ErrSecretValueTooLarge(api.Limits{SecretValueMaxBytes: maxValueBytes}, len(value))
	}
	return Seal(recipient, Envelope{key: value})
}

// SealBytes seals an arbitrary plaintext blob (issue #396 / ADR-045
// PR 3 webhook_secret + PR 4 meterd-side dispatcher reads). The
// blob is NOT wrapped in an Envelope map and is NOT validated
// against the env-var key regex — the bytes are opaque to apid and
// only the dispatcher needs to be able to recover them. The
// maxValueBytes byte cap is enforced here so a megabyte secret
// payload can't blow up the age writer.
//
// namespace is recorded in the seal metadata so a future OpenBytes
// can disambiguate (currently unused but the prefix is future-
// proofing for multi-secret-per-blob cases). Pass "" for no
// namespace; the namespace MUST be lowercase + no whitespace.
func SealBytes(recipient *age.X25519Recipient, namespace string, plaintext []byte, maxValueBytes int) ([]byte, error) {
	if recipient == nil {
		return nil, errors.New("secretbox: nil recipient")
	}
	if maxValueBytes > 0 && len(plaintext) > maxValueBytes {
		return nil, api.ErrSecretValueTooLarge(api.Limits{SecretValueMaxBytes: maxValueBytes}, len(plaintext))
	}
	// Prepend the namespace as an ASCII tag so OpenBytes can later
	// distinguish alert-rule-secret blobs from app-secret blobs in
	// the unlikely event both land in the same column. The tag is
	// authenticated by ChaCha20-Poly1305 inside the age stanza; a
	// tampered tag fails Open.
	tag := ""
	if namespace != "" {
		tag = namespace + "\x00"
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, recipient)
	if err != nil {
		return nil, fmt.Errorf("secretbox: open age writer: %w", err)
	}
	if _, err := w.Write([]byte(tag)); err != nil {
		return nil, fmt.Errorf("secretbox: write namespace tag: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("secretbox: write plaintext: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("secretbox: close age writer: %w", err)
	}
	return out.Bytes(), nil
}

// OpenBytes is the inverse of SealBytes. Returns the namespace +
// the original plaintext. The age decryption authenticates the
// ciphertext + the namespace tag together; a tampered tag fails
// the open.
func OpenBytes(identity *age.X25519Identity, blob []byte) (namespace string, plaintext []byte, err error) {
	if identity == nil {
		return "", nil, errors.New("secretbox: nil identity")
	}
	if len(blob) == 0 {
		return "", nil, errors.New("secretbox: empty blob")
	}
	r, err := age.Decrypt(bytes.NewReader(blob), identity)
	if err != nil {
		return "", nil, fmt.Errorf("secretbox: open age reader: %w", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", nil, fmt.Errorf("secretbox: read plaintext: %w", err)
	}
	// Split on the first NUL. If no NUL, namespace is empty.
	for i, b := range raw {
		if b == 0 {
			return string(raw[:i]), raw[i+1:], nil
		}
	}
	return "", raw, nil
}
