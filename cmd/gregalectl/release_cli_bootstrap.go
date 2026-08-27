package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

// releaseCLIAlreadyBootstrapped is an internal process marker. It is passed
// only to the child CLI extracted from the already verified release, so the
// child can run the requested command instead of recursively extracting and
// executing itself.
const releaseCLIAlreadyBootstrapped = "GREGALE_RELEASE_CLI_BOOTSTRAPPED"

// releaseBootstrapVerifier and releaseCLICommandRunner are seams for tests.
// Production uses the strict GitHub OIDC verifier and execs the verified
// release binary with the caller's standard streams attached.
var releaseBootstrapVerifier = func() releaseinstall.CosignVerifier {
	return releaseinstall.NewExecCosignVerifier(releaseinstall.DefaultGitHubOIDC())
}

var releaseCLICommandRunner = runReleaseCLICommand

// maybeBootstrapReleaseCLI is called before command dispatch. A release
// install or provider-neutral node join may be started with a gregalectl from
// an older release. Those binaries cannot validate a manifest that adds a new
// support executable. The signed release carries the matching gregalectl, so
// use it after verifying the archive and its exact member hash.
func maybeBootstrapReleaseCLI(args []string) (int, bool) {
	if os.Getenv(releaseCLIAlreadyBootstrapped) == "1" {
		return 0, false
	}
	// Help and `deploy join-* --dry-run` are non-mutating operations. They
	// must not verify or execute a release CLI, because doing so would turn
	// a read-only request into an artifact-dependent operation.
	if releaseBootstrapIsPlanning(args) {
		return 0, false
	}
	tarballPath, expectedGitSHA, ok := releaseBootstrapRequest(args)
	if !ok || tarballPath == "" {
		return 0, false
	}
	return maybeBootstrapReleaseCLIFromTarball(tarballPath, expectedGitSHA)
}

func releaseBootstrapIsPlanning(args []string) bool {
	if len(args) < 2 {
		return false
	}
	for _, arg := range args[2:] {
		if arg == "--help" || arg == "-h" {
			return true
		}
		if arg == "--dry-run" && args[0] == "deploy" && (args[1] == "join-node" || args[1] == "join-fleet") {
			return true
		}
	}
	return false
}

// maybeBootstrapReleaseCLIFromTarball covers artifact-dir joins, where the
// command-line tarball flag is filled in after the top-level dispatcher has
// already run. It intentionally uses os.Args for the child so global flags
// such as --json are preserved exactly.
func maybeBootstrapReleaseCLIFromTarball(tarballPath, expectedGitSHA string) (int, bool) {
	if os.Getenv(releaseCLIAlreadyBootstrapped) == "1" || strings.TrimSpace(tarballPath) == "" {
		return 0, false
	}
	code, err := bootstrapReleaseCLI(tarballPath, expectedGitSHA, os.Args[1:])
	if err != nil {
		if code < 3 {
			code = 3
		}
		_, _ = fmt.Fprintf(os.Stderr, "gregalectl: release CLI bootstrap: %v\n", err)
	}
	return code, true
}

// releaseBootstrapRequest returns the signed tarball flag for commands whose
// first operation must be performed by the catalog-aware release CLI. The
// parser is deliberately small and non-mutating; the leaf command remains
// responsible for normal flag validation and diagnostics.
func releaseBootstrapRequest(args []string) (tarballPath, expectedGitSHA string, ok bool) {
	if len(args) < 2 {
		return "", "", false
	}
	var tarballFlag, shaFlag string
	switch {
	case args[0] == "release" && args[1] == "install":
		tarballFlag, shaFlag = "--tarball-path", "--git-sha"
	case args[0] == "deploy" && (args[1] == "join-node" || args[1] == "join-fleet"):
		tarballFlag, shaFlag = "--release-tarball", "--release-git-sha"
	default:
		return "", "", false
	}
	return releaseFlagValue(args[2:], tarballFlag), releaseFlagValue(args[2:], shaFlag), true
}

func releaseFlagValue(args []string, name string) string {
	for i, arg := range args {
		if arg == name {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

// bootstrapReleaseCLI verifies the signed archive before executing anything
// from it. The full manifest and archive validation remains in the child
// release CLI; this parent-side check is the minimum trust boundary needed to
// safely obtain that child when the parent is from an older release.
func bootstrapReleaseCLI(tarballPath, expectedGitSHA string, args []string) (int, error) {
	if strings.TrimSpace(tarballPath) == "" {
		return 3, errors.New("empty release tarball path")
	}
	if info, err := os.Lstat(tarballPath); err != nil {
		return 3, fmt.Errorf("stat tarball: %w", err)
	} else if !info.Mode().IsRegular() {
		return 3, fmt.Errorf("tarball is not a regular file: %s", tarballPath)
	}
	sigPath, err := releaseAssetPath(tarballPath, releaseSigName)
	if err != nil {
		return 3, err
	}
	if info, err := os.Lstat(sigPath); err != nil {
		return 3, fmt.Errorf("stat cosign bundle: %w", err)
	} else if !info.Mode().IsRegular() {
		return 3, fmt.Errorf("cosign bundle is not a regular file: %s", sigPath)
	}

	packed, err := os.ReadFile(tarballPath)
	if err != nil {
		return 3, fmt.Errorf("read tarball: %w", err)
	}
	sigBody, err := os.ReadFile(sigPath)
	if err != nil {
		return 3, fmt.Errorf("read cosign bundle: %w", err)
	}
	tmpRoot, err := os.MkdirTemp("", "gregalectl-release-bootstrap-")
	if err != nil {
		return 3, fmt.Errorf("create CLI bootstrap directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpRoot) }()
	// Verify private copies of the bytes we will parse and execute. This
	// avoids a replace-between-check-and-use race on either operator-supplied
	// artifact path.
	verifyTarballPath := filepath.Join(tmpRoot, releaseTarballName)
	verifySigPath := filepath.Join(tmpRoot, releaseSigName)
	if err := os.WriteFile(verifyTarballPath, packed, 0o600); err != nil {
		return 3, fmt.Errorf("stage tarball for verification: %w", err)
	}
	if err := os.WriteFile(verifySigPath, sigBody, 0o600); err != nil {
		return 3, fmt.Errorf("stage cosign bundle for verification: %w", err)
	}
	verifier := releaseBootstrapVerifier()
	if verifier == nil {
		return 3, errors.New("release verifier is nil")
	}
	// Verify the complete signed archive before extracting the executable.
	if _, err := verifier.VerifyBlob(context.Background(), verifyTarballPath, verifySigPath); err != nil {
		return 3, fmt.Errorf("verify signed release: %w", err)
	}

	manifestBytes, err := extractTarballMember(packed, releaseinstall.ManifestName)
	if err != nil {
		return 3, fmt.Errorf("read embedded release manifest: %w", err)
	}
	var releaseManifest releaseinstall.Manifest
	if err := json.Unmarshal(manifestBytes, &releaseManifest); err != nil {
		return 3, fmt.Errorf("decode embedded release manifest: %w", err)
	}
	if releaseManifest.FormatVersion != releaseinstall.FormatVersion {
		return 3, fmt.Errorf("unsupported release manifest format %d", releaseManifest.FormatVersion)
	}
	if !releaseinstall.ValidGitSHA(releaseManifest.GitSHA) {
		return 3, fmt.Errorf("embedded release git_sha %q is invalid", releaseManifest.GitSHA)
	}
	if expectedGitSHA != "" && releaseManifest.GitSHA != expectedGitSHA {
		return 3, fmt.Errorf("embedded release git_sha=%s does not match requested %s", releaseManifest.GitSHA, expectedGitSHA)
	}

	const cliName = "gregalectl"
	want, ok := releaseManifest.ToolHashes[cliName]
	if !ok {
		return 3, errors.New("embedded release manifest does not contain tool_hashes.gregalectl")
	}
	wantHex, err := releaseHashHex(want)
	if err != nil {
		return 3, fmt.Errorf("embedded gregalectl hash: %w", err)
	}
	cliBytes, err := extractTarballMember(packed, cliName)
	if err != nil {
		return 3, fmt.Errorf("extract embedded gregalectl: %w", err)
	}
	got := sha256.Sum256(cliBytes)
	gotHex := hex.EncodeToString(got[:])
	if gotHex != wantHex {
		return 3, fmt.Errorf("embedded gregalectl sha256=%s does not match manifest %s", gotHex, want)
	}

	cliPath := filepath.Join(tmpRoot, cliName)
	if err := os.WriteFile(cliPath, cliBytes, 0o700); err != nil {
		return 3, fmt.Errorf("stage embedded gregalectl: %w", err)
	}
	if err := os.Chmod(cliPath, 0o700); err != nil {
		return 3, fmt.Errorf("make embedded gregalectl executable: %w", err)
	}

	return runReleaseCLIChild(cliPath, args)
}

func releaseHashHex(value string) (string, error) {
	hexValue, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(hexValue) != sha256.Size*2 {
		return "", fmt.Errorf("%q is not sha256:<64hex>", value)
	}
	if _, err := hex.DecodeString(hexValue); err != nil {
		return "", fmt.Errorf("%q is not sha256:<64hex>: %w", value, err)
	}
	return hexValue, nil
}

func runReleaseCLIChild(path string, args []string) (int, error) {
	env := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, releaseCLIAlreadyBootstrapped+"=") {
			continue
		}
		env = append(env, value)
	}
	env = append(env, releaseCLIAlreadyBootstrapped+"=1")
	err := releaseCLICommandRunner(path, args, env)
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 3, fmt.Errorf("execute embedded gregalectl: %w", err)
}

func runReleaseCLICommand(path string, args []string, env []string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
