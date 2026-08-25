package renderer

import (
	"fmt"
	"strings"
)

// renderCgroupBody builds the subtree_control body bytes for the
// per-host slice. The slice's write happens in renderer.go's publish phase;
// cgroup v2's subtree_control is a kernel pseudo-file and is therefore
// written directly rather than through the regular atomic-file publisher.
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

	// Build the canonical space-separated "+ctrl +ctrl" body. The cgroup v2
	// kernel interface accepts whitespace-separated operations, but does not
	// accept one operation per line. Sorted output keeps re-renders stable.
	ordered := make([]string, 0, len(parsed))
	for c := range parsed {
		ordered = append(ordered, c)
	}
	sortStrings(ordered)
	var body strings.Builder
	for i, c := range ordered {
		if i > 0 {
			body.WriteByte(' ')
		}
		body.WriteString("+")
		body.WriteString(c)
	}
	body.WriteByte('\n')
	return []byte(body.String()), nil
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
