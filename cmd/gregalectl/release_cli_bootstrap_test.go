package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/manifest"
	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

type recordingReleaseVerifier struct {
	called bool
}

func (v *recordingReleaseVerifier) VerifyBlob(_ context.Context, tarballPath, sigPath string) (string, error) {
	v.called = true
	if tarballPath == "" || sigPath == "" {
		return "", errors.New("missing verifier paths")
	}
	return "test-identity", nil
}

func TestReleaseBootstrapRequest(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		tarball  string
		gitSHA   string
		wantOkay bool
	}{
		{
			name:     "release install equals flags",
			args:     []string{"release", "install", "--git-sha=abc", "--tarball-path=/tmp/release.tar.gz"},
			tarball:  "/tmp/release.tar.gz",
			gitSHA:   "abc",
			wantOkay: true,
		},
		{
			name:     "join node separate flags",
			args:     []string{"deploy", "join-node", "--release-tarball", "/tmp/release.tar.gz", "--release-git-sha", "abc"},
			tarball:  "/tmp/release.tar.gz",
			gitSHA:   "abc",
			wantOkay: true,
		},
		{
			name:     "unrelated command",
			args:     []string{"doctor", "--release-tarball=/tmp/release.tar.gz"},
			wantOkay: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTarball, gotSHA, gotOkay := releaseBootstrapRequest(tc.args)
			if gotTarball != tc.tarball || gotSHA != tc.gitSHA || gotOkay != tc.wantOkay {
				t.Fatalf("releaseBootstrapRequest() = (%q, %q, %v), want (%q, %q, %v)",
					gotTarball, gotSHA, gotOkay, tc.tarball, tc.gitSHA, tc.wantOkay)
			}
		})
	}
}

func TestReleaseBootstrapIsPlanning(t *testing.T) {
	for _, args := range [][]string{
		{"release", "install", "--help", "--tarball-path=/tmp/release.tar.gz"},
		{"deploy", "join-node", "--dry-run", "--release-tarball=/tmp/release.tar.gz"},
		{"deploy", "join-fleet", "-h", "--release-tarball=/tmp/release.tar.gz"},
	} {
		if !releaseBootstrapIsPlanning(args) {
			t.Errorf("releaseBootstrapIsPlanning(%v) = false, want true", args)
		}
	}
	if releaseBootstrapIsPlanning([]string{"deploy", "join-node", "--release-tarball=/tmp/release.tar.gz", "--yes"}) {
		t.Fatal("releaseBootstrapIsPlanning(apply) = true, want false")
	}
}

func TestBootstrapReleaseCLI_VerifiesAndExecutesEmbeddedBinary(t *testing.T) {
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	root := t.TempDir()
	binDir := releaseinstall.BinDir(root, gitSHA)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.SortedHostKeys() {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("daemon-"+name), 0o755); err != nil {
			t.Fatalf("write daemon %s: %v", name, err)
		}
	}
	cliBytes := []byte("catalog-aware-gregalectl")
	if err := os.WriteFile(filepath.Join(binDir, "gregalectl"), cliBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	tb, err := releaseinstall.BuildTarball(root, gitSHA, "sha256:"+strings.Repeat("a", 64), time.Now())
	if err != nil {
		t.Fatalf("build tarball: %v", err)
	}
	stage := t.TempDir()
	tarballPath := filepath.Join(stage, releaseTarballName)
	if err := os.WriteFile(tarballPath, tb.Packed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, releaseSigName), []byte("signed"), 0o644); err != nil {
		t.Fatal(err)
	}

	verifier := &recordingReleaseVerifier{}
	previousVerifier := releaseBootstrapVerifier
	previousRunner := releaseCLICommandRunner
	t.Cleanup(func() {
		releaseBootstrapVerifier = previousVerifier
		releaseCLICommandRunner = previousRunner
	})
	releaseBootstrapVerifier = func() releaseinstall.CosignVerifier { return verifier }
	var gotPath string
	var gotArgs []string
	var gotEnv []string
	releaseCLICommandRunner = func(path string, args []string, env []string) error {
		gotPath = path
		gotArgs = append([]string(nil), args...)
		gotEnv = append([]string(nil), env...)
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(body, cliBytes) {
			return errors.New("runner received unexpected CLI bytes")
		}
		return nil
	}

	wantArgs := []string{"release", "install", "--git-sha", gitSHA, "--tarball-path", tarballPath}
	code, err := bootstrapReleaseCLI(tarballPath, gitSHA, wantArgs)
	if err != nil {
		t.Fatalf("bootstrapReleaseCLI: %v", err)
	}
	if code != 0 {
		t.Fatalf("bootstrapReleaseCLI exit code = %d, want 0", code)
	}
	if !verifier.called {
		t.Fatal("signed release verifier was not called")
	}
	if gotPath == "" {
		t.Fatal("embedded CLI was not executed")
	}
	if gotPath == filepath.Join(stage, "gregalectl") {
		t.Fatal("embedded CLI was not staged in a private temporary directory")
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("child args = %v, want %v", gotArgs, wantArgs)
	}
	foundGuard := false
	for _, value := range gotEnv {
		if value == releaseCLIAlreadyBootstrapped+"=1" {
			foundGuard = true
			break
		}
	}
	if !foundGuard {
		t.Fatalf("child environment does not contain recursion guard %s=1", releaseCLIAlreadyBootstrapped)
	}
}

func TestBootstrapReleaseCLI_RequiresEmbeddedBinary(t *testing.T) {
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	root := t.TempDir()
	binDir := releaseinstall.BinDir(root, gitSHA)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.SortedHostKeys() {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("daemon-"+name), 0o755); err != nil {
			t.Fatalf("write daemon %s: %v", name, err)
		}
	}
	tb, err := releaseinstall.BuildTarball(root, gitSHA, "sha256:"+strings.Repeat("a", 64), time.Now())
	if err != nil {
		t.Fatalf("build tarball: %v", err)
	}
	stage := t.TempDir()
	tarballPath := filepath.Join(stage, releaseTarballName)
	if err := os.WriteFile(tarballPath, tb.Packed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, releaseSigName), []byte("signed"), 0o644); err != nil {
		t.Fatal(err)
	}
	previousVerifier := releaseBootstrapVerifier
	previousRunner := releaseCLICommandRunner
	t.Cleanup(func() {
		releaseBootstrapVerifier = previousVerifier
		releaseCLICommandRunner = previousRunner
	})
	releaseBootstrapVerifier = func() releaseinstall.CosignVerifier { return &recordingReleaseVerifier{} }
	releaseCLICommandRunner = func(_ string, _ []string, _ []string) error {
		t.Fatal("runner called without embedded gregalectl")
		return nil
	}

	if _, err := bootstrapReleaseCLI(tarballPath, gitSHA, nil); err == nil || !strings.Contains(err.Error(), "does not contain tool_hashes.gregalectl") {
		t.Fatalf("bootstrapReleaseCLI error = %v, want missing embedded CLI", err)
	}
}

func TestReleaseHashHex(t *testing.T) {
	good := "sha256:" + strings.Repeat("a", 64)
	if got, err := releaseHashHex(good); err != nil || got != strings.Repeat("a", 64) {
		t.Fatalf("releaseHashHex(good) = (%q, %v)", got, err)
	}
	for _, bad := range []string{"", "sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("z", 64), "md5:" + strings.Repeat("a", 64)} {
		if _, err := releaseHashHex(bad); err == nil {
			t.Fatalf("releaseHashHex(%q) = nil error, want rejection", bad)
		}
	}
}
