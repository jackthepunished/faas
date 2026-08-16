package secretbox

import (
	"crypto/sha256"
	"os"
	"sync"
)

// HostHashSalt (ADR-098 §D1.b / §11) is the §11-barrier helper that
// hashes a plaintext host into the 64-hex `host_redacted_hash`
// value the data_upstreams + data_upstream_probes tables index on.
// Plaintext host NEVER leaves the handler — the on-wire shape
// (Prom labels, audit kinds, pg_notify payload, schedd affinity
// map key) is the hash. The hash is the ONLY permitted
// host-derived label on the metrics + audit surface.
//
// The salt is loaded from the same deploy-time secret directory as
// the host identity (hostkey.go's /etc/faas/secrets/host.age) and
// cached in process memory behind a sync.Once. A miss on the salt
// file is a fatal boot-time error — the absence of the salt means
// the cluster cannot enforce §11, and the right thing is to refuse
// to start (an InsecureSkip-style fallback would let a misconfigured
// operator's plaintext host land in Prom labels).
//
// The salt is per-cluster (not per-host or per-app), so a single
// salt file ships with the cluster and is rotated alongside
// host.age. Rotation is a separate concern (ADR-098 §D6 leans
// toward not-rotating-the-salt because the hash is one-way;
// rotation would orphan every existing data_upstreams row).
//
// The hash function is sha256(salt || host) — NOT bcrypt, NOT
// scrypt, NOT argon2id. The hash is NOT a password — it has to be
// computable at probe-loop speed (C5 runs the hash on every probe
// sample, ~30s × N rows) and the host is already in cleartext at
// the apid capture site (D1.b). The salting prevents a hash
// lookup table from leaking the host registry; the per-row salt
// would prevent a salt leak from leaking the host, but a per-row
// salt is incompatible with the prom-label cardinality story
// (every new sample would mint a new salt). sha256(salt || host)
// is the right primitive for this surface.
//
// __unsalted__ sentinel: the migration CHECK at
// `data_upstreams_host_redacted_hash_check` accepts the literal
// `__unsalted__` as a test-fixture-only escape hatch (PR-A's
// `migrations/00226_data_upstreams_test.go` uses it to seed rows
// without going through the live salt). The writer at
// `cmd/apid/extract.go` (C4) NEVER stamps `__unsalted__` — the
// quiescence grep at `pkg/data/quiescence_secret_rule_test.go`
// (C4) is the tripwire that gates any future regression.

// hostHashSalt is the process-cached salt. Loaded once at first
// call to HostHashSalt via the lazy initializer below.
var (
	hostHashSaltOnce sync.Once
	hostHashSalt     []byte
	hostHashSaltErr  error
	// hostHashSaltPath is the on-disk salt path. Default is
	// /etc/faas/secrets/host_hash_salt; tests override via
	// SetHostHashSaltPath + ResetHostHashSaltCache. Production
	// callers never touch this — it's package-private.
	hostHashSaltPath = "/etc/faas/secrets/host_hash_salt"
)

// HostHashSalt returns the 32-byte salt used to hash plaintext
// hosts into the §11 barrier value. The salt is loaded from
// /etc/faas/secrets/host_hash_salt on first call; the file must
// contain 32 raw bytes (NOT hex-encoded). A miss is a fatal
// error — see the package-level docstring for the security
// rationale.
//
// The returned slice is the cached process-internal copy; callers
// MUST NOT mutate it.
func HostHashSalt() ([]byte, error) {
	hostHashSaltOnce.Do(func() {
		hostHashSalt, hostHashSaltErr = loadHostHashSalt()
	})
	return hostHashSalt, hostHashSaltErr
}

// ResetHostHashSaltCache clears the sync.Once + cache so a
// subsequent HostHashSalt call re-reads from disk. Used by
// tests; production code MUST NOT call this. The path must be
// re-set first via SetHostHashSaltPath.
func ResetHostHashSaltCache() {
	hostHashSaltOnce = sync.Once{}
	hostHashSalt = nil
	hostHashSaltErr = nil
}

// SetHostHashSaltPath overrides the on-disk salt path. Used by
// tests to point at t.TempDir(); production code MUST NOT call
// this. Caller MUST also call ResetHostHashSaltCache before the
// next HostHashSalt call.
func SetHostHashSaltPath(path string) {
	hostHashSaltPath = path
}

// loadHostHashSalt reads the configured salt path (default
// /etc/faas/secrets/host_hash_salt). The file is 32 raw bytes
// (generated at bootstrap time via
// `openssl rand -out host_hash_salt 32`). Returns a copy so the
// on-disk file can be re-read without affecting the cached value.
func loadHostHashSalt() ([]byte, error) {
	data, err := os.ReadFile(hostHashSaltPath)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 {
		return nil, &HostHashSaltError{
			Path:   hostHashSaltPath,
			GotLen: len(data),
		}
	}
	// Copy so a future re-read of the file doesn't see the
	// cache mutated externally.
	out := make([]byte, 32)
	copy(out, data)
	return out, nil
}

// HashHost returns the 64-hex `host_redacted_hash` value for a
// plaintext host. The host is hashed with sha256(salt || host)
// and the result is lower-case hex. Convenience wrapper over
// HostHashSalt + sha256 that the apid classifier (C4) and meterd
// probe loop (C5) call per row.
//
// The function is deterministic — same (salt, host) returns the
// same hash. That's the contract: schedd's affinity map keys on
// the hash and must produce identical scores for identical
// samples.
func HashHost(host string) (string, error) {
	salt, err := HostHashSalt()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	// sha256.New never returns an error from Write.
	_, _ = h.Write(salt)
	_, _ = h.Write([]byte(host))
	sum := h.Sum(nil)
	return hexEncodeLower(sum), nil
}

// HostHashSaltError is returned when the salt file is missing or
// has the wrong length. Distinct from a generic I/O error so
// apid's startup log can render "host_hash_salt missing or wrong
// size — refusing to start" without parsing prose.
type HostHashSaltError struct {
	Path   string
	GotLen int
}

func (e *HostHashSaltError) Error() string {
	return "secretbox: " + e.Path + " must be exactly 32 bytes (got " +
		intToA(e.GotLen) + "); see pkg/secretbox/host_hash.go for the bootstrap procedure"
}

// hexEncodeLower returns the lower-case hex encoding of b. Local
// to this file so we don't pull encoding/hex into the import
// block of pkg/secretbox/seal.go.
func hexEncodeLower(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
}

// intToA is a zero-alloc int-to-decimal helper to keep the
// error-message path allocation-free.
func intToA(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
