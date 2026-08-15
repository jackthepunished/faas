package renderer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// renderCgroupBody builds the subtree_control body bytes for the
// per-host slice. The slice's actual mkdir + write happens in
// renderer.go's publish phase (so the SHA256 idempotent short-
// circuit covers cgroup writes too).
//
// The `memory` controller is load-bearing for tenant admission
// (CLAUDE.md / §11). The renderer asserts it is present in the
// manifest's `controllers` list and refuses to publish if it is
// absent — there's no migration path that lets the renderer recover
// from a missing memory controller (the daemon would refuse to start
// anyway).
func renderCgroupBody(sliceName, controllers string) ([]byte, error) {
	if sliceName == "" {
		return nil, fmt.Errorf("renderer: cgroup: empty slice name")
	}
	parsed := parseControllers(controllers)
	if !parsed["memory"] {
		return nil, fmt.Errorf("renderer: cgroup: controllers list %q missing \"memory\" (load-bearing for tenant admission; CLAUDE.md §11)", controllers)
	}
	if !parsed["cpu"] {
		return nil, fmt.Errorf("renderer: cgroup: controllers list %q missing \"cpu\"", controllers)
	}

	// Build the "+ctrl +ctrl" body. Sorted so a re-render produces
	// the same bytes (idempotent short-circuit).
	ordered := make([]string, 0, len(parsed))
	for c := range parsed {
		ordered = append(ordered, c)
	}
	sortStrings(ordered)
	var body strings.Builder
	for _, c := range ordered {
		body.WriteString("+")
		body.WriteString(c)
		body.WriteByte('\n')
	}
	return []byte(body.String()), nil
}

// renderCgroup ensures the slice directory exists and publishes
// the subtree_control body via publishAtomic. The slice mkdir is
// not part of the idempotent short-circuit (it's a no-op once the
// slice exists) and runs before publish so publishAtomic sees a
// usable target path.
func renderCgroup(rootDir, sliceName, controllers string) error {
	body, err := renderCgroupBody(sliceName, controllers)
	if err != nil {
		return err
	}
	slicePath := filepath.Join(rootDir, sliceName)
	if err := os.MkdirAll(slicePath, 0o755); err != nil {
		return fmt.Errorf("renderer: cgroup: mkdir %s: %w", slicePath, err)
	}
	ctrlPath := filepath.Join(slicePath, "subtree_control")
	if _, _, err := publishAtomic(ctrlPath, body, 0o644); err != nil {
		return fmt.Errorf("renderer: cgroup: write %s: %w", ctrlPath, err)
	}
	return nil
}

// parseControllers splits the manifest's `controllers` string into a
// set. Tolerant of whitespace and trailing commas; rejects empty
// tokens.
func parseControllers(controllers string) map[string]bool {
	out := make(map[string]bool)
	parts := strings.Split(controllers, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out[p] = true
	}
	return out
}

// sortStrings is a tiny strings-sort helper to avoid pulling in
// the `sort` package dependency for a one-line use. The renderer
// only sorts string keys here.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
