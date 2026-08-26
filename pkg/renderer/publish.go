// Package renderer is the manifest → on-disk emitter for issue #911 /
// ADR-110. It consumes a validated `pkg/manifest.Manifest` and
// produces the per-host artifacts that the daemons need to run:
// /etc/faas/<daemon>.toml, /etc/systemd/system/faas-<daemon>.service,
// the faas-cp.slice unit, cgroup v2 subtree_control delegation, and
// the per-daemon PKI leaves under /etc/faas/tls/.
//
// Regular outputs are published atomically (tmp + rename). The cgroup v2
// subtree_control pseudo-file is the one intentional exception: kernel
// control files cannot be replaced with rename(2), so it is updated through
// its write interface. Every output remains idempotent.
//
// Scope (PR-2): the entry point + the five sub-renderers. The
// release-bundle install path lives under `gregale release install`
// (PR-3), not `gregale manifest install` — the renderer is the
// read-side of the cluster ship flow, the install is the write-side.
package renderer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// publishAtomic writes body to path atomically: tmp file in the same
// directory (so the rename is on the same filesystem), chmod 0o644,
// sync, close, then os.Rename over the target. Returns the sha256 hex
// digest of body on success. Caller must NOT pre-create path: the
// renderer is the sole writer for these five outputs.
//
// Idempotent: if the existing file at path has the same content as
// body (compared via sha256), the publish is a no-op and the existing
// file is left untouched. The renderer returns the digest and
// `changed = false` in that case so the caller can stamp
// OutputReport.Action = "skipped" vs "wrote".
//
// tmp file path uses os.CreateTemp in the parent directory so the
// rename is on the same filesystem (no cross-device rename). The
// tmp file is removed on any error path to avoid leaving detritus
// on the host filesystem.
func publishAtomic(path string, body []byte, mode os.FileMode) (digest string, changed bool, err error) {
	digest = sha256Hex(body)

	// Idempotent short-circuit: if the existing file matches body,
	// skip the publish. operator-initiated `--force` re-writes would
	// be a separate flag (deferred).
	if existing, statErr := os.ReadFile(path); statErr == nil {
		if sha256Hex(existing) == digest {
			return digest, false, nil
		}
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("renderer: read existing %s: %w", path, statErr)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, fmt.Errorf("renderer: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".render-*.tmp")
	if err != nil {
		return "", false, fmt.Errorf("renderer: create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: chmod %s: %w", tmpPath, err)
	}
	if _, err = tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: write %s: %w", tmpPath, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("renderer: sync %s: %w", tmpPath, err)
	}
	if err = tmp.Close(); err != nil {
		return "", false, fmt.Errorf("renderer: close %s: %w", tmpPath, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return "", false, fmt.Errorf("renderer: rename %s → %s: %w", tmpPath, path, err)
	}
	return digest, true, nil
}

// publishCgroupControl updates a cgroup v2 control pseudo-file in place.
// cgroupfs does not permit creating a temporary sibling or replacing a
// pseudo-file with rename(2). The kernel applies the complete write as the
// control-file update, so opening the target directly is the correct
// operation for this interface.
//
// The read side reports enabled controllers without the '+' prefix while the
// write side requires it. Compare controller sets rather than raw bytes so a
// successful prior write is idempotent across those two representations.
func publishCgroupControl(path string, body []byte) (digest string, changed bool, err error) {
	digest = sha256Hex(body)

	if existing, readErr := os.ReadFile(path); readErr == nil {
		if cgroupControlSatisfies(existing, body) {
			return digest, false, nil
		}
	} else if !os.IsNotExist(readErr) {
		return "", false, fmt.Errorf("renderer: read cgroup control %s: %w", path, readErr)
	}

	control, err := openCgroupControl(path)
	if err != nil {
		return "", false, fmt.Errorf("renderer: open cgroup control %s: %w", path, err)
	}
	if _, err = control.Write(body); err != nil {
		_ = control.Close()
		return "", false, fmt.Errorf("renderer: write cgroup control %s: %w", path, err)
	}
	if err = control.Close(); err != nil {
		return "", false, fmt.Errorf("renderer: close cgroup control %s: %w", path, err)
	}
	return digest, true, nil
}

// ensureCgroupControllers enables the requested controllers through the
// systemd cgroup hierarchy so they are available at targetDir. cgroup v2
// exposes a controller to a child only when every ancestor has enabled it in
// its own cgroup.subtree_control. The renderer owns this small delegation
// step because systemd may start with a reduced DefaultControllers set.
//
// A filesystem-backed test root has no cgroup.controllers file, so it is
// intentionally left alone; the normal publisher remains usable there.
func ensureCgroupControllers(cgroupRoot, targetDir string, desired []byte) error {
	controllersPath := filepath.Join(cgroupRoot, "cgroup.controllers")
	if _, err := os.Stat(controllersPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("renderer: stat cgroup controllers %s: %w", controllersPath, err)
	}

	rel, err := filepath.Rel(cgroupRoot, targetDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("renderer: cgroup target %s escapes root %s", targetDir, cgroupRoot)
	}
	wanted := cgroupControllerSet(desired)
	current := cgroupRoot
	if rel != "." {
		for _, component := range strings.Split(rel, string(filepath.Separator)) {
			if component == "" || component == "." {
				continue
			}
			if err := enableCgroupControllers(current, wanted); err != nil {
				return err
			}
			current = filepath.Join(current, component)
		}
	}

	availableBody, err := os.ReadFile(filepath.Join(current, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("renderer: read cgroup controllers %s: %w", current, err)
	}
	available := cgroupControllerSet(availableBody)
	for controller := range wanted {
		if _, ok := available[controller]; !ok {
			return fmt.Errorf("renderer: cgroup controller %q unavailable at %s", controller, current)
		}
	}
	return nil
}

func enableCgroupControllers(cgroupDir string, wanted map[string]struct{}) error {
	availableBody, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("renderer: read cgroup controllers %s: %w", cgroupDir, err)
	}
	available := cgroupControllerSet(availableBody)
	enabledBody, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("renderer: read cgroup subtree control %s: %w", cgroupDir, err)
	}
	enabled := cgroupControllerSet(enabledBody)

	var missing []string
	for controller := range wanted {
		if _, ok := available[controller]; !ok {
			return fmt.Errorf("renderer: cgroup controller %q unavailable at %s", controller, cgroupDir)
		}
		if _, ok := enabled[controller]; !ok {
			missing = append(missing, controller)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sortStrings(missing)
	var body strings.Builder
	for i, controller := range missing {
		if i > 0 {
			body.WriteByte(' ')
		}
		body.WriteByte('+')
		body.WriteString(controller)
	}
	body.WriteByte('\n')
	control, err := openCgroupControl(filepath.Join(cgroupDir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("renderer: open cgroup subtree control %s: %w", cgroupDir, err)
	}
	if _, err := control.Write([]byte(body.String())); err != nil {
		_ = control.Close()
		return fmt.Errorf("renderer: enable cgroup controllers at %s: %w", cgroupDir, err)
	}
	if err := control.Close(); err != nil {
		return fmt.Errorf("renderer: close cgroup subtree control %s: %w", cgroupDir, err)
	}
	return nil
}

func openCgroupControl(path string) (*os.File, error) {
	flags := os.O_WRONLY
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// Filesystem-backed renderer tests use an ordinary file tree. Real
		// cgroupfs pseudo-files already exist and take the non-create path.
		flags |= os.O_CREATE
	}
	return os.OpenFile(path, flags, 0o644)
}

func cgroupControllerSet(body []byte) map[string]struct{} {
	out := make(map[string]struct{})
	for _, token := range strings.Fields(string(body)) {
		token = strings.TrimPrefix(token, "+")
		if token != "" {
			out[token] = struct{}{}
		}
	}
	return out
}

func cgroupControlSatisfies(existing, desired []byte) bool {
	available := cgroupControllerSet(existing)
	for token := range cgroupControllerSet(desired) {
		if _, ok := available[token]; !ok {
			return false
		}
	}
	return true
}

// sha256Hex returns the lowercase hex sha256 of body. Mirrors
// pkg/releaseinstall.hashFile's encoding for wire compatibility.
func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
