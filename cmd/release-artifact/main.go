// release-artifact produces the unsigned, deterministic portion of the
// canonical daemon release. Signing is deliberately left to the CI job so
// cosign's keyless OIDC identity is the GitHub workflow that built the bytes.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/onebox-faas/faas/pkg/releaseinstall"
)

func main() {
	fs := flag.NewFlagSet("release-artifact", flag.ExitOnError)
	root := fs.String("root", "", "staging root containing <git-sha>/bin (required)")
	gitSHA := fs.String("git-sha", "", "40-character release commit SHA (required)")
	manifestHash := fs.String("manifest-hash", "", "deployment manifest hash sha256:<64hex> (required)")
	outDir := fs.String("out-dir", "out", "artifact output directory")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *root == "" || *gitSHA == "" || *manifestHash == "" {
		fs.Usage()
		os.Exit(2)
	}

	tb, err := releaseinstall.BuildTarball(*root, *gitSHA, *manifestHash, time.Unix(0, 0).UTC())
	if err != nil {
		fatal(err)
	}
	if err := releaseinstall.Write(*root, tb.Manifest); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*outDir, "release.tar.gz"), tb.Packed, 0o644); err != nil {
		fatal(err)
	}
	manifestBody, err := os.ReadFile(filepath.Join(releaseinstall.BundleRoot(*root, *gitSHA), releaseinstall.ManifestName))
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*outDir, releaseinstall.ManifestName), manifestBody, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s and %s\n", filepath.Join(*outDir, "release.tar.gz"), filepath.Join(*outDir, releaseinstall.ManifestName))
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "release-artifact: %v\n", err)
	os.Exit(1)
}
