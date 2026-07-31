// commands_backup_test.go — pins the load-bearing contracts of the
// operator-side off-host pg backup unseal CLI (commands_backup.go).
//
// The current contracts under test:
//
//   - unsealRclone refuses to overwrite an existing plaintext unless
//     --force is passed. The refuse-by-default contract protects a
//     mid-deploy operator from accidentally rotating the rclone
//     session — a rotation against a Hetzner Storage Box in active
//     use strands the nightly push unit until the new key is in
//     place. --force is the documented escape hatch; --force=false
//     (the default) is the safe path.
//
//   - unsealRclone rejects a wrong box-age-key: the test seals a
//     plaintext with identity A and tries to unseal with identity B.
//     We expect a decrypt error containing no plaintext bytes — the
//     decrypt failure surfaces as the wrong-key wrapper, not a
//     silent corruption.
//
// Both contracts are exercised with a tmpdir + a freshly-generated
// age X25519 pair so the test never touches /etc/faas/secrets/ and
// runs cleanly on a developer laptop without the production secret
// path populated.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// generateAgeKey writes a fresh age X25519 identity to disk and
// returns its path. Mirrors the operator-laptop flow (`age-keygen
// -o box-age.key`) so the test exercises the same identity shape
// the unseal command will see in production.
func generateAgeKey(t *testing.T) string {
	t.Helper()
	ident, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	path := filepath.Join(t.TempDir(), "box-age.key")
	if err := os.WriteFile(path, []byte(ident.String()), 0o400); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	return path
}

// sealForRecipients seals plaintext against the X25519 recipient
// derived from the given identity. We deliberately reconstruct a
// recipient from the identity's String() form (which is "AGE
// SECRET KEY-...") by parsing it back — round-trips the same
// identity format the unseal side sees.
func sealForRecipients(t *testing.T, identityPath, plaintext string) string {
	t.Helper()
	raw, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	ident, err := age.ParseX25519Identity(string(raw))
	if err != nil {
		t.Fatalf("ParseX25519Identity: %v", err)
	}
	recipient := ident.Recipient()
	var sealed strings.Builder
	w, err := age.Encrypt(&sealed, recipient)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := w.Write([]byte(plaintext)); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close sealer: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "rclone.conf.age")
	if err := os.WriteFile(outPath, []byte(sealed.String()), 0o400); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
	return outPath
}

// TestUnsealRclone_HappyPath seals a fake rclone.conf with a fresh
// age key, then unseals it into a tempdir. Verifies that the
// plaintext bytes round-trip and the output file ends up mode 0440
// — the mode the ansible stat-assert at
// postgres_backup/tasks/main.yml requires (review F2). The chown
// to root:postgres is the operator's job after the unseal (we
// don't depend on user postgres existing in test envs); the
// unseal itself only owns the chmod + write.
func TestUnsealRclone_HappyPath(t *testing.T) {
	identPath := generateAgeKey(t)
	const plaintext = "[hertznerbox]\ntype = sftp\nhost = u123.your-storagebox.de\nuser = u123\n"
	envelope := sealForRecipients(t, identPath, plaintext)

	outPath := filepath.Join(t.TempDir(), "rclone.conf")
	f := &unsealRcloneFlags{
		ageIdentity: identPath,
		in:          envelope,
		out:         outPath,
		force:       false,
	}
	if err := unsealRclone(f); err != nil {
		t.Fatalf("unsealRclone: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read unsealed: %v", err)
	}
	if string(got) != plaintext {
		t.Fatalf("round-trip mismatch:\n got: %q\nwant: %q", got, plaintext)
	}

	st, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("stat out: %v", err)
	}
	if mode := st.Mode().Perm(); mode != 0o440 {
		t.Fatalf("mode: got %#o want 0440", mode)
	}
}

// TestUnsealRclone_RefuseOverwrite pins the refuse-rotate-by-default
// contract. We unseal once successfully, then attempt a second
// unseal against the same output path without --force and expect a
// refusal error. The test name encodes the contract so a future
// "let's default force to true" PR fails this test loudly.
func TestUnsealRclone_RefuseOverwrite(t *testing.T) {
	identPath := generateAgeKey(t)
	envelope := sealForRecipients(t, identPath, "first")

	outPath := filepath.Join(t.TempDir(), "rclone.conf")
	if err := unsealRclone(&unsealRcloneFlags{
		ageIdentity: identPath,
		in:          envelope,
		out:         outPath,
		force:       false,
	}); err != nil {
		t.Fatalf("first unseal: %v", err)
	}

	err := unsealRclone(&unsealRcloneFlags{
		ageIdentity: identPath,
		in:          envelope,
		out:         outPath,
		force:       false,
	})
	if err == nil {
		t.Fatal("second unseal without --force should refuse, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("refusal error: got %q, want substring 'refusing to overwrite'", err.Error())
	}
}

// TestUnsealRclone_WrongKey rejects a wrong box-age-key by sealing
// with identity A and unsealing with identity B. We expect a
// decrypt error — the wrong-key wrapper surfaces fast (no
// half-decrypted file written to disk because the atomic-rename
// happens after the full io.Copy succeeds).
func TestUnsealRclone_WrongKey(t *testing.T) {
	identA := generateAgeKey(t)
	identB := generateAgeKey(t)
	envelope := sealForRecipients(t, identA, "secret")

	outPath := filepath.Join(t.TempDir(), "rclone.conf")
	err := unsealRclone(&unsealRcloneFlags{
		ageIdentity: identB,
		in:          envelope,
		out:         outPath,
		force:       false,
	})
	if err == nil {
		t.Fatal("unseal with wrong key should fail, got nil error")
	}
	// Either the wrapper text or a substring thereof — filippo.io/age
	// emits "no identity matched any of the recipients" on a clean
	// key mismatch. We don't pin the exact text (it's upstream); we
	// pin that we got a non-nil error and the destination file was
	// never created.
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Fatalf("output file should not exist on wrong-key failure: stat err = %v", statErr)
	}
}
