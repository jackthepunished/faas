package markers

import "strings"

// appMarker is one row of the closed set of top-level filenames
// that identify a project as a supported build pipeline. The
// order in appMarkers IS THE PRIORITY ORDER — Dockerfile is
// checked first so it beats package.json / go.mod etc. when
// both are root-level. Go's map iteration is non-deterministic,
// so this is a slice, not a map.
//
// Adding a new marker (e.g. Cargo.toml for a Rust Railpack
// pipeline) is a one-line change here. Both the CLI and the
// server pick it up automatically. See ADR-088 §3.
type appMarker struct {
	filename  string
	framework Framework
}

// appMarkers is the priority-ordered marker list. Mirrors the
// case statements at pkg/builderd/detect.go:73-82 (now
// superseded). Case-insensitive matching applies at the call
// site. Priority order:
//
//	Docker > Node > Python > Go
//
// matches pkg/builderd.Detect's prior behaviour and the
// TestDetect_PythonBeatsGo / TestDetect_DockerfileBeatsGo pins.
var appMarkers = []appMarker{
	{"Dockerfile", FrameworkDocker},
	{"package.json", FrameworkNode},
	{"requirements.txt", FrameworkPython},
	{"pyproject.toml", FrameworkPython},
	{"Pipfile", FrameworkPython},
	{"setup.py", FrameworkPython},
	{"go.mod", FrameworkGo},
}

// MarkerFor returns the framework for a single top-level
// filename, or FrameworkUnknown if name is not a marker.
// Case-insensitive — callers do not need to pre-normalise.
// Used by the CLI's detectShape (replacing the inline switch)
// and detectNestedMarkerHint/walkForMarkers (replacing the
// local appMarker map).
func MarkerFor(name string) Framework {
	lower := strings.ToLower(name)
	for _, m := range appMarkers {
		if strings.ToLower(m.filename) == lower {
			return m.framework
		}
	}
	return FrameworkUnknown
}

// IsAppMarker reports whether name is any app marker (excludes
// handler.* and any non-marker file). Used by the CLI's shape
// detector and the depth-2 hint walker — see ADR-086 + ADR-088.
func IsAppMarker(name string) bool {
	return MarkerFor(name) != FrameworkUnknown
}

// Markers returns the closed set of marker filenames in priority
// order. The slice is freshly allocated on every call so callers
// can mutate without affecting the package state. Used for
// documentation / tests; runtime callers should use MarkerFor
// or IsAppMarker.
func Markers() []string {
	out := make([]string, len(appMarkers))
	for i, m := range appMarkers {
		out[i] = m.filename
	}
	return out
}
