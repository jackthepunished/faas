package builderd

import (
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestMapFramework pins the host-side Framework → api.BuildFramework
// translation. Issue #54: docker must round-trip to FrameworkDockerfile so
// guest-init dispatches to buildctl --frontend dockerfile (ADR-004), not
// to Railpack --plan auto.
func TestMapFramework(t *testing.T) {
	cases := []struct {
		name string
		in   Framework
		want api.BuildFramework
	}{
		{"node", FrameworkNode, api.FrameworkRailpackNode},
		{"python", FrameworkPython, api.FrameworkRailpackPython},
		{"go", FrameworkGo, api.FrameworkRailpackGo},
		{"docker → dockerfile (buildctl)", FrameworkDocker, api.FrameworkDockerfile},
		{"unknown falls back to auto", FrameworkUnknown, api.FrameworkAuto},
		{"empty string falls back to auto", Framework(""), api.FrameworkAuto},
		{"garbage falls back to auto", Framework("rust"), api.FrameworkAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MapFramework(tc.in); got != tc.want {
				t.Errorf("MapFramework(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMapFramework_DockerIsNotRailpack is the issue #54 acceptance guard:
// the only thing this test asserts is that FrameworkDocker never maps to
// anything Railpack-shaped. If this regresses, `faas deploy --dockerfile`
// silently falls back to Railpack.
func TestMapFramework_DockerIsNotRailpack(t *testing.T) {
	got := MapFramework(FrameworkDocker)
	switch got {
	case api.FrameworkRailpackNode, api.FrameworkRailpackPython, api.FrameworkRailpackGo, api.FrameworkAuto:
		t.Fatalf("FrameworkDocker mapped to %q (Railpack fallback) — expected FrameworkDockerfile (buildctl)", got)
	}
	if got != api.FrameworkDockerfile {
		t.Fatalf("FrameworkDocker mapped to %q, want %q", got, api.FrameworkDockerfile)
	}
}

// TestMapFramework_GoIsNotAuto is the Go-side acceptance guard: a Go
// tarball (go.mod at root) must dispatch to railpack_go explicitly, not
// fall through to FrameworkAuto. If this regresses, every Go deploy
// hands Railpack the wrong plan and silently builds the wrong image
// (or fails inside the VM with a Railpack heuristic error that the
// customer can't act on). Parallel in shape to the Docker guard above.
func TestMapFramework_GoIsNotAuto(t *testing.T) {
	got := MapFramework(FrameworkGo)
	switch got {
	case api.FrameworkRailpackNode, api.FrameworkRailpackPython, api.FrameworkDockerfile, api.FrameworkAuto:
		t.Fatalf("FrameworkGo mapped to %q (non-Go fallback) — expected FrameworkRailpackGo", got)
	}
	if got != api.FrameworkRailpackGo {
		t.Fatalf("FrameworkGo mapped to %q, want %q", got, api.FrameworkRailpackGo)
	}
}
