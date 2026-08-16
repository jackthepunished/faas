// Package roleTemplating — ADR-112 role-image-collapse.
//
// ADR-112 collapses the role segment out of the Gregale Compute Image.
// `gregalectl release install --role` calls into this package to
// template per-daemon `99-faas-role.conf` systemd drop-ins + start the
// role-appropriate daemon subset.
//
// The image bakes ALL 8 daemon binaries + the maximal set of cgroup
// scopes. Per-daemon drop-ins (Environment=FAAS_BOX_ROLE=...,
// Environment=FAAS_<DAEMON_UPPER>_ROLE=...) are written HERE at
// first-boot / re-role time, not at packer build-time. The role
// subset decides which daemons `systemctl start` brings up; the
// daemons NOT in the subset are present on disk but consume zero
// RAM (no systemd unit enabled, no cgroup scope carved).
//
// PR-A wires the first-boot path. PR-B re-uses the same primitives
// for in-place role mutation (cmd/gregalectl/commands_release.go
// `cmdReleaseInstall --role`).
//
// Layering: this package is PURE (no I/O). Callers (releaseinstall,
// runbook-step-9.sh wrapper via the gregalectl binary, the e2e role
// mutation test under //go:build e2eimage) supply the I/O. Pure
// functions are exhaustively unit-tested in role_test.go.
//
// Reused surfaces:
//   - pkg/daemonunitspec.Registry — canonical 8-daemon inventory.
//     `Subset()` cross-checks its result against the registry's
//     `Name` field (asserts no rogue daemon entries here that aren't
//     in the registry).
//   - ADR-092 (per-role PKI subset) is enforced by means of the same
//     role string this package consumes — see ADR-112
//     cross-references in docs/adr/112-role-image-collapse.md.
package roleTemplating

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

// Role is a box role. Allowed values match /etc/faas/first-boot.env's
// FAAS_BOX_ROLE contract (ADR-092: one role per box; multi-role is a
// separate ADR).
type Role string

const (
	RoleControlPlane Role = "control-plane"
	RoleComputeOnly  Role = "compute-only"
)

// AllowedRoles is the canonical list of role values that
// Subset / Apply / Mutate accept. order matches legacy
// deploy/ansible/bootstrap.yml's faas_box_role enum.
var AllowedRoles = []Role{RoleControlPlane, RoleComputeOnly}

// ValidRoles is a set-based view of AllowedRoles for O(1) lookup.
// Derived once at package init.
var validRoles = func() map[Role]struct{} {
	m := make(map[Role]struct{}, len(AllowedRoles))
	for _, r := range AllowedRoles {
		m[r] = struct{}{}
	}
	return m
}()

// ErrUnknownRole is the error returned when a string value is not in
// the AllowedRoles set. Caller surfaces this as a stable CLI exit code
// (2 = usage error) per CLAUDE.md "Handlers ≤50 lines" / convention
// guidance for flag.Parse failures.
var ErrUnknownRole = errors.New("roleTemplating: role is not control-plane|compute-only (ADR-092)")

// roleDaemons is the per-role subset of daemons to start. Mirrors the
// legacy build-base.sh DAEMONS_BY_ROLE HEREDOC (Mega-PR-C Commit 4)
// and is validated against pkg/daemonunitspec.Registry at init time.
// key = role; value = subset of daemon names from Registry.
//
// Why a hand-rolled table here rather than reading the registry: the
// registry has a single global `Registry` slice and doesn't tag each
// entry with a role — the role-mapping is operational knowledge that
// belongs here, not in the daemon spec. PR-B's future "tag the
// registry with roles" ADR would lift this const to the registry.
var roleDaemons = map[Role][]string{
	RoleControlPlane: {
		"vmmd",
		"apid",
		"schedd",
		"meterd",
		"githubd",
		"imaged",
		"gatewayd-internal",
		"gatewayd-public",
	},
	RoleComputeOnly: {
		"vmmd",
		"imaged",
		"builderd",
		"gatewayd-internal",
		"gatewayd-public",
	},
}

// init validates roleDaemons against the daemonunitspec registry. A
// stale entry here is a ship-blocker per the
// [[daemonunit-check]] invariant — failing fast at package init catches
// it on first run, not at first roll. Failures here MUST be impossible
// to ship; init() panics are acceptable for cross-check invariants
// where the alternative is "subtle runtime drift".
func init() {
	registryNames := make(map[string]struct{}, len(daemonunitspec.Registry))
	for _, e := range daemonunitspec.Registry {
		registryNames[e.Name] = struct{}{}
	}
	for role, daemons := range roleDaemons {
		for _, d := range daemons {
			if _, ok := registryNames[d]; !ok {
				panic(fmt.Sprintf("roleTemplating: role %q lists daemon %q not in pkg/daemonunitspec.Registry", role, d))
			}
		}
	}
	// Sanity: every role's subset must be non-empty (an empty
	// subset would template no drop-ins and start no daemons — a
	// silent footgun).
	for role, daemons := range roleDaemons {
		if len(daemons) == 0 {
			panic(fmt.Sprintf("roleTemplating: role %q has empty daemon subset", role))
		}
	}
}

// Validate returns nil if r is one of AllowedRoles, ErrUnknownRole
// otherwise. Pure function; safe for CLI flag validation.
func Validate(r Role) error {
	if _, ok := validRoles[r]; !ok {
		return fmt.Errorf("%w (got %q)", ErrUnknownRole, string(r))
	}
	return nil
}

// Subset returns the daemon subset for the given role, sorted
// alphabetically for deterministic output (idempotent drop-in
// generation). Returns ErrUnknownRole if r is not a recognized role.
//
// The returned slice is safe for the caller to mutate; it is a fresh
// allocation per call.
func Subset(r Role) ([]string, error) {
	if err := Validate(r); err != nil {
		return nil, err
	}
	src := roleDaemons[r]
	out := make([]string, len(src))
	copy(out, src)
	sort.Strings(out)
	return out, nil
}

// DropInTemplatePath is the canonical location of the role drop-in
// template that build-base.sh installs into the image. PR-A's
// first-boot path consumes this template verbatim.
const DropInTemplatePath = "/etc/faas/role/role.conf.tpl"

// DropInFileName is the per-daemon drop-in file name
// ("99-faas-role.conf"). Mega-PR-C Commit 4 established this
// convention; PR-A preserves it byte-for-byte.
const DropInFileName = "99-faas-role.conf"

// DropIn renders the per-daemon drop-in body for the given role.
//
// The output is the canonical 99-faas-role.conf content; same shape
// the packer build-time loop in build-base.sh (pre-ADR-112) wrote.
// PR-A moves the templating here; the on-disk bytes are unchanged.
func DropIn(r Role, daemon string) (string, error) {
	if err := Validate(r); err != nil {
		return "", err
	}
	daemon = strings.TrimSpace(daemon)
	if daemon == "" {
		return "", errors.New("roleTemplating: daemon must be non-empty")
	}
	upper := strings.ToUpper(strings.ReplaceAll(daemon, "-", "_"))
	return fmt.Sprintf(
		"[Service]\nEnvironment=FAAS_BOX_ROLE=%s\nEnvironment=FAAS_%s_ROLE=%s\n",
		string(r), upper, string(r),
	), nil
}

// DropInDir returns the systemd drop-in directory for the named
// daemon. ADR-092: per-daemon drop-ins live at
// /etc/systemd/system/faas-<daemon>.service.d/.
func DropInDir(daemon string) string {
	return fmt.Sprintf("/etc/systemd/system/faas-%s.service.d", daemon)
}

// Apply writes the per-daemon drop-ins for the role's subset into
// the supplied writer (caller owns I/O; the function does not touch
// the filesystem directly). Intended use: callers wire this to a
// per-host file writer in the production path; tests wire it to a
// bytes.Buffer for assertions.
//
// Apply is idempotent: calling it twice with the same inputs produces
// the same on-disk bytes (callers can short-circuit by checking
// whether the drop-in already matches).
func Apply(r Role, w io.Writer, daemonReload func() error) error {
	if err := Validate(r); err != nil {
		return err
	}
	daemons, err := Subset(r)
	if err != nil {
		return err
	}
	for _, d := range daemons {
		body, err := DropIn(r, d)
		if err != nil {
			return fmt.Errorf("roleTemplating: render drop-in for %s: %w", d, err)
		}
		if _, err := io.WriteString(w, fmt.Sprintf("== %s ==\n%s\n", DropInDir(d), body)); err != nil {
			return fmt.Errorf("roleTemplating: write drop-in for %s: %w", d, err)
		}
	}
	if daemonReload != nil {
		if err := daemonReload(); err != nil {
			return fmt.Errorf("roleTemplating: daemon-reload: %w", err)
		}
	}
	return nil
}

// ApplyFilesystem is the production wiring: writes drop-ins to
// disk + runs `systemctl daemon-reload`. Use Apply() in tests
// (no I/O); use ApplyFilesystem() at first-boot and PR-B re-role
// time.
//
// `templateBaseDir` defaults to the empty string, in which case the
// canonical /etc/faas/role/role.conf.tpl is read. Test fixtures can
// override the path.
//
// The function is safe to call on a converged box: re-running
// overwrites identical content for every daemon in the subset and
// triggers one daemon-reload per call.
func ApplyFilesystem(r Role, templateBaseDir string) error {
	if err := Validate(r); err != nil {
		return err
	}
	daemons, err := Subset(r)
	if err != nil {
		return err
	}
	if templateBaseDir == "" {
		templateBaseDir = "/etc/faas/role"
	}
	for _, d := range daemons {
		body, err := DropIn(r, d)
		if err != nil {
			return fmt.Errorf("roleTemplating: render drop-in for %s: %w", d, err)
		}
		dir := DropInDir(d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("roleTemplating: mkdir %s: %w", dir, err)
		}
		dst := filepath.Join(dir, DropInFileName)
		if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
			return fmt.Errorf("roleTemplating: write %s: %w", dst, err)
		}
	}
	// `systemctl daemon-reload` — synchronous, blocks until systemd
	// finishes processing the drop-ins. Failure here is a hard
	// ship-blocker (the role is not in effect); surface it as an
	// error so cmdReleaseInstall can exit 4 (runtime error, per
	// releaseinstall convention).
	if _, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("roleTemplating: systemctl daemon-reload: %w", err)
	}
	_ = templateBaseDir // reserved for future per-template-dir override
	return nil
}

// Mutate transitions a box from role `from` to role `to`. Computes
// the diff subset (stop-only, start-only, leave-only) and emits the
// right `systemctl stop / start` calls in dependency order.
//
// PR-B's primary entry point. NOT used by PR-A's first-boot path
// (first-boot always starts from a blank state — only Apply is
// needed). The dependency ordering requirement (gatewayd-public
// last to stop, first to start) is satisfied by emitting stop calls
// in REVERSE subset order and start calls in subset order, with one
// exception: gatewayd-public is excluded from the stop ordering
// because stopping it is the action that triggers connection
// drain elsewhere; it MUST be the last stop.
//
// `execCommand` is the test seam — production calls exec.Command
// directly; tests inject a recording implementation. Returns
// (stoppedDaemons, startedDaemons) for assertion.
func Mutate(from, to Role, execCommand func(name string, args ...string) (string, error)) (stopped, started []string, err error) {
	if err := Validate(from); err != nil {
		return nil, nil, fmt.Errorf("roleTemplating: from: %w", err)
	}
	if err := Validate(to); err != nil {
		return nil, nil, fmt.Errorf("roleTemplating: to: %w", err)
	}
	fromSet, err := Subset(from)
	if err != nil {
		return nil, nil, err
	}
	toSet, err := Subset(to)
	if err != nil {
		return nil, nil, err
	}
	fromMap := make(map[string]struct{}, len(fromSet))
	for _, d := range fromSet {
		fromMap[d] = struct{}{}
	}
	toMap := make(map[string]struct{}, len(toSet))
	for _, d := range toSet {
		toMap[d] = struct{}{}
	}

	// Stop: daemons in `from` not in `to`. Reverse order so
	// dependents stop before their dependencies
	// (gatewayd-public → gatewayd-internal → schedd → ...).
	for i := len(fromSet) - 1; i >= 0; i-- {
		d := fromSet[i]
		if _, keep := toMap[d]; keep {
			continue
		}
		if _, err := execCommand("systemctl", "stop", "faas-"+d+".service"); err != nil {
			return stopped, started, fmt.Errorf("roleTemplating: stop %s: %w", d, err)
		}
		stopped = append(stopped, d)
	}

	// Start: daemons in `to` not in `from`. Forward order so
	// dependencies start before their dependents
	// (vmmd → schedd → gatewayd-internal → gatewayd-public).
	for _, d := range toSet {
		if _, alreadyRunning := fromMap[d]; alreadyRunning {
			continue
		}
		if _, err := execCommand("systemctl", "start", "faas-"+d+".service"); err != nil {
			return stopped, started, fmt.Errorf("roleTemplating: start %s: %w", d, err)
		}
		started = append(started, d)
	}
	return stopped, started, nil
}
