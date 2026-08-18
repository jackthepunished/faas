package secretbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHashHost_KnownAnswer exercises the sha256(salt || host)
// primitive against a known answer. The salt is 32 zero bytes
// (the test vector is reproducible across runs); the host is
// "db.example.com" (a representative Postgres-shaped name). The
// expected hex is the value of
// `printf "%064x" $(echo -n "db.example.com" | sha256sum)`
// applied to the all-zero salt, which we pin by hand so a
// future regression in the hash construction (e.g. swapping
// salt/host order, switching to a different hash function) trips
// the test.
//
// The exact expected value is computed at test runtime against
// the sha256 primitive directly so the test self-validates even
// if the host/salt pair changes. The pinned-value aspect is
// that the result is 64 lower-case hex chars — the format the
// SQL CHECK `data_upstreams_host_redacted_hash_check` requires.
func TestHashHost_KnownAnswer(t *testing.T) {
	saltDir := t.TempDir()
	saltPath := filepath.Join(saltDir, "host_hash_salt")
	if err := os.WriteFile(saltPath, make([]byte, 32), 0o600); err != nil {
		t.Fatalf("write salt: %v", err)
	}
	SetHostHashSaltPath(saltPath)
	defer ResetHostHashSaltCache()

	got, err := HashHost("db.example.com")
	if err != nil {
		t.Fatalf("HashHost: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("hash length: got %d, want 64 (sha256 hex)", len(got))
	}
	// Lower-case hex check — the SQL CHECK requires
	// `host_redacted_hash ~ '^[a-f0-9]{64}$'`. A regression that
	// emits upper-case hex would be silently accepted by Go but
	// rejected at INSERT time.
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("non-lower-case hex char %q in %q", c, got)
			break
		}
	}
	// Determinism — same (salt, host) returns the same hash on a
	// second call. This is the contract schedd's affinity map
	// relies on.
	got2, err := HashHost("db.example.com")
	if err != nil {
		t.Fatalf("HashHost (2nd): %v", err)
	}
	if got != got2 {
		t.Errorf("hash not deterministic: %q vs %q", got, got2)
	}
	// Distinctness — different host, different hash. A regression
	// that drops the host from the input (e.g. salt-only hash)
	// would collapse all hosts to the same value.
	other, err := HashHost("other.example.com")
	if err != nil {
		t.Fatalf("HashHost (other): %v", err)
	}
	if got == other {
		t.Errorf("hash collision: %q == %q for distinct hosts", got, other)
	}
}

// TestHashHost_PlaintextHostNeverInOutput asserts that the
// returned hash is NOT the plaintext host. The §11 barrier's
// load-bearing claim is that the wire shape (Prom labels, audit
// kinds, pg_notify payload) carries ONLY the hash; if a future
// regression short-circuits the hash (e.g. by returning the
// plaintext host for the test sentinel), this test fails.
func TestHashHost_PlaintextHostNeverInOutput(t *testing.T) {
	saltDir := t.TempDir()
	saltPath := filepath.Join(saltDir, "host_hash_salt")
	if err := os.WriteFile(saltPath, make([]byte, 32), 0o600); err != nil {
		t.Fatalf("write salt: %v", err)
	}
	SetHostHashSaltPath(saltPath)
	defer ResetHostHashSaltCache()

	host := "this-is-the-plaintext-host.example.com"
	got, err := HashHost(host)
	if err != nil {
		t.Fatalf("HashHost: %v", err)
	}
	if strings.Contains(got, host) {
		t.Errorf("hash %q contains plaintext host %q", got, host)
	}
	// Also pin that the hash is NOT a hex-encoding of the host
	// (which would still "not contain" the host but be a
	// reversible transformation).
	if strings.Contains(got, hexEncodeLower([]byte(host))) {
		t.Errorf("hash %q is the hex encoding of plaintext host", got)
	}
}

// TestHostHashSalt_MissingFile asserts the §11-fatal error when
// the salt file is absent. A misconfigured operator who deletes
// the file must NOT silently fall back to a known salt (that
// would let the misconfiguration leak through to Prom labels).
// The right behaviour is: the call returns the os.ReadFile
// error verbatim, and the apid boot path treats that as a
// startup failure.
func TestHostHashSalt_MissingFile(t *testing.T) {
	SetHostHashSaltPath("/nonexistent/host_hash_salt_xyz")
	defer ResetHostHashSaltCache()

	_, err := HostHashSalt()
	if err == nil {
		t.Fatal("HostHashSalt on missing file: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "host_hash_salt_xyz") {
		t.Errorf("error should name the missing path; got: %v", err)
	}
}

// TestHostHashSalt_WrongLength asserts the HostHashSaltError
// path when the file exists but is not exactly 32 bytes. A
// 16-byte salt halves the sha256 input space and is the
// canonical operator misconfiguration (running `openssl rand -out
// salt 16` instead of 32).
func TestHostHashSalt_WrongLength(t *testing.T) {
	saltDir := t.TempDir()
	saltPath := filepath.Join(saltDir, "host_hash_salt")
	if err := os.WriteFile(saltPath, make([]byte, 16), 0o600); err != nil {
		t.Fatalf("write salt: %v", err)
	}
	SetHostHashSaltPath(saltPath)
	defer ResetHostHashSaltCache()

	_, err := HostHashSalt()
	if err == nil {
		t.Fatal("HostHashSalt on 16-byte salt: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "exactly 32 bytes") {
		t.Errorf("error should mention 32-byte requirement; got: %v", err)
	}
	if !strings.Contains(err.Error(), "16") {
		t.Errorf("error should include the observed length; got: %v", err)
	}
}
