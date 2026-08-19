package releaseinstall

import (
	"os"
	"path/filepath"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// supportBinaryNames are executable files that are not daemons but are
// required by a running host. They travel with the atomic release because
// vmmd starts the bridge helpers and the upgrade path invokes gregalectl from
// the active release tree.
var supportBinaryNames = []string{
	"gregale",
	"gregalectl",
	"vmmd-raw-bridge",
	"vmmd-stream-bridge",
}

// SupportBinaryNames returns the fixed support-binary catalog in stable order.
func SupportBinaryNames() []string {
	return append([]string(nil), supportBinaryNames...)
}

// executableName translates the manifest's logical daemon key to the
// filename emitted by the Go build and consumed by systemd. The manifest
// uses underscores for YAML/TOML map keys, while the two gateway binaries
// use dashes everywhere else in the deployment tree.
func executableName(logical string) string {
	switch logical {
	case "gatewayd_internal":
		return "gatewayd-internal"
	case "gatewayd_public":
		return "gatewayd-public"
	default:
		return logical
	}
}

// IsCatalogBinaryName reports whether name is either a logical catalog key
// or the executable filename for a catalog daemon. Release bundle inputs may
// come from the canonical daemon build (dashed gateway names) or from the
// package-level tests/older tooling (logical underscored names).
func IsCatalogBinaryName(name string) bool {
	for _, logical := range manifest.SortedHostKeys() {
		if name == logical || name == executableName(logical) {
			return true
		}
	}
	return false
}

// IsReleaseBinaryName reports whether name is a daemon or a required support
// executable in the immutable release bundle.
func IsReleaseBinaryName(name string) bool {
	if IsCatalogBinaryName(name) {
		return true
	}
	for _, support := range supportBinaryNames {
		if name == support {
			return true
		}
	}
	return false
}

// resolveBinary returns the on-disk file for a logical daemon key. It
// accepts the historical logical filename as a compatibility path, but
// prefers the canonical executable filename when that is the only one
// present. This keeps the manifest keys stable while making the release
// bundle agree with systemd and the build scripts.
func resolveBinary(bin, logical string) (string, error) {
	candidates := []string{logical}
	if canonical := executableName(logical); canonical != logical {
		candidates = append(candidates, canonical)
	}
	var firstErr error
	for _, candidate := range candidates {
		path := filepath.Join(bin, candidate)
		info, err := os.Stat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", &os.PathError{Op: "stat", Path: path, Err: os.ErrInvalid}
			}
			return path, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return filepath.Join(bin, candidates[0]), firstErr
}
