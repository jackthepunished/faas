// Package markers is the single source of truth for the
// top-level-filename → build-framework switch used by both the
// CLI (cmd/gregale) and the build server (pkg/builderd). The
// marker list and priority order live here; the two callers
// import this package instead of duplicating the switch. The
// version parser (VersionFromFS / VersionFromTarball) lives
// here too — ADR-087's CLI mirror is gone.
//
// See ADR-088 for the design.
package markers

// Framework names the inferred build pipeline. The CLI prints
// this in the "Detected: app, framework=<fw>" banner; the
// server records it on build_provenance.framework.
type Framework string

const (
	FrameworkNode    Framework = "node"
	FrameworkPython  Framework = "python"
	FrameworkGo      Framework = "go"
	FrameworkDocker  Framework = "docker"
	FrameworkUnknown Framework = "unknown"
)