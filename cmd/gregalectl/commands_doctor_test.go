// commands_doctor_test.go — focused tests for the doctor's
// secrets check (PR-X / issue #911 / ADR-110).
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeTempSecretsDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("test requires root (symlink /etc/faas/secrets + chmod 0400)")
	}
	dir := t.TempDir()
	storageDir := filepath.Join(dir, "storage-box")
	if err := os.MkdirAll(storageDir, 0o750); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	if err := os.Symlink(dir, "/etc/faas/secrets"); err != nil {
		t.Skipf("symlink /etc/faas/secrets failed: %v (test env already has secrets dir; refusing to clobber)", err)
	}
	t.Cleanup(func() { _ = os.Remove("/etc/faas/secrets") })
	return dir
}

func writeSecretFile(t *testing.T, path string, body []byte, mode string) {
	t.Helper()
	perm := mustParsePerm(mode)
	if err := os.WriteFile(path, body, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCheckSecrets_AllPresentNoDrift(t *testing.T) {
	dir := makeTempSecretsDir(t)
	storageDir := filepath.Join(dir, "storage-box")
	writeSecretFile(t, "/etc/faas/secrets/host.age", []byte("placeholder host.age bytes"), "0400")
	writeSecretFile(t, "/etc/faas/secrets/session.key", []byte(strings.Repeat("a", 64)), "0400")
	writeSecretFile(t, filepath.Join(storageDir, "box-age-key"), []byte("placeholder box-age-key"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "rclone.conf"), []byte("[remote]\ntype = s3\n"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "archive-creds.json"), []byte(`{"bucket":"prod-archive"}`), "0400")
	deps := &doctorDeps{}
	findings, err := checkSecrets(context.Background(), deps)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	if len(findings) != 0 {
		for _, f := range findings {
			t.Errorf("unexpected finding: %+v", f)
		}
	}
}

func TestCheckSecrets_MissingHostAge(t *testing.T) {
	dir := makeTempSecretsDir(t)
	storageDir := filepath.Join(dir, "storage-box")
	writeSecretFile(t, "/etc/faas/secrets/session.key", []byte(strings.Repeat("a", 64)), "0400")
	writeSecretFile(t, filepath.Join(storageDir, "box-age-key"), []byte("placeholder"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "rclone.conf"), []byte(`{}`), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "archive-creds.json"), []byte(`{}`), "0400")
	deps := &doctorDeps{}
	findings, err := checkSecrets(context.Background(), deps)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	gotMissing := false
	for _, f := range findings {
		if f.Message == "missing host.age" {
			gotMissing = true
			if f.Severity != doctorSeverityError {
				t.Errorf("missing host.age severity = %s, want error", f.Severity)
			}
		}
	}
	if !gotMissing {
		t.Errorf("missing host.age finding not emitted (got: %+v)", findings)
	}
}

func TestCheckSecrets_WrongHostAgeMode(t *testing.T) {
	dir := makeTempSecretsDir(t)
	storageDir := filepath.Join(dir, "storage-box")
	writeSecretFile(t, "/etc/faas/secrets/host.age", []byte("placeholder"), "0644")
	writeSecretFile(t, "/etc/faas/secrets/session.key", []byte(strings.Repeat("a", 64)), "0400")
	writeSecretFile(t, filepath.Join(storageDir, "box-age-key"), []byte("placeholder"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "rclone.conf"), []byte(`{}`), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "archive-creds.json"), []byte(`{}`), "0400")
	deps := &doctorDeps{}
	findings, err := checkSecrets(context.Background(), deps)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	gotModeErr := false
	for _, f := range findings {
		if strings.HasPrefix(f.Message, "host.age wrong mode") {
			gotModeErr = true
		}
	}
	if !gotModeErr {
		t.Errorf("host.age wrong mode finding not emitted (got: %+v)", findings)
	}
}

func TestCheckSecrets_SessionKeyNotHex(t *testing.T) {
	dir := makeTempSecretsDir(t)
	storageDir := filepath.Join(dir, "storage-box")
	writeSecretFile(t, "/etc/faas/secrets/host.age", []byte("placeholder"), "0400")
	writeSecretFile(t, "/etc/faas/secrets/session.key", []byte("zzz"+strings.Repeat("a", 61)), "0400")
	writeSecretFile(t, filepath.Join(storageDir, "box-age-key"), []byte("placeholder"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "rclone.conf"), []byte(`{}`), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "archive-creds.json"), []byte(`{}`), "0400")
	deps := &doctorDeps{}
	findings, err := checkSecrets(context.Background(), deps)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	gotFormatErr := false
	for _, f := range findings {
		if f.Message == "session.key is not valid hex" {
			gotFormatErr = true
		}
	}
	if !gotFormatErr {
		t.Errorf("session.key not-hex finding not emitted (got: %+v)", findings)
	}
}

func TestCheckSecrets_SessionKeyWrongLength(t *testing.T) {
	dir := makeTempSecretsDir(t)
	storageDir := filepath.Join(dir, "storage-box")
	writeSecretFile(t, "/etc/faas/secrets/host.age", []byte("placeholder"), "0400")
	writeSecretFile(t, "/etc/faas/secrets/session.key", []byte(strings.Repeat("a", 32)), "0400")
	writeSecretFile(t, filepath.Join(storageDir, "box-age-key"), []byte("placeholder"), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "rclone.conf"), []byte(`{}`), "0440")
	writeSecretFile(t, filepath.Join(storageDir, "archive-creds.json"), []byte(`{}`), "0400")
	deps := &doctorDeps{}
	findings, err := checkSecrets(context.Background(), deps)
	if err != nil {
		t.Fatalf("checkSecrets: %v", err)
	}
	gotLengthErr := false
	for _, f := range findings {
		if strings.HasPrefix(f.Message, "session.key wrong length") {
			gotLengthErr = true
		}
	}
	if !gotLengthErr {
		t.Errorf("session.key wrong-length finding not emitted (got: %+v)", findings)
	}
}
