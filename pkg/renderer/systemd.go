package renderer

import (
	"fmt"

	"github.com/onebox-faas/faas/pkg/daemonunitspec"
)

// renderSystemd returns the bytes for the per-daemon systemd unit file
// at /etc/systemd/system/faas-<daemon>.service. The renderer does NOT
// construct the daemonunit.Unit — pkg/daemonunitspec.UnitXxx() is the
// single source of truth for the per-daemon directives. The renderer
// only routes the call to the correct constructor and serialises via
// pkg/daemonunit.Unit.Render().
//
// The 8 daemonunitspec.UnitXxx() functions live in
// pkg/daemonunitspec/{vmmd,apid,schedd,gatewayd_internal,
// gatewayd_public,meterd,githubd,imaged}.go. The renderer looks the
// daemon up by name (the manifest's daemon name with `_` → `-`
// flipped for the two gatewayd daemons, matching the cli convention).
func renderSystemd(daemon string) ([]byte, error) {
	unit, err := daemonunitspec.UnitByName(daemon)
	if err != nil {
		return nil, fmt.Errorf("renderer: %s: %w", daemon, err)
	}
	return unit.Render(), nil
}

// renderSliceUnit returns the bytes for /etc/systemd/system/faas-cp.slice.
// The slice is emitted once per host (it is the wrapper for all 8
// daemons, not a daemon itself). pkg/daemonunitspec.UnitSlice() is the
// source of truth.
func renderSliceUnit() []byte {
	return daemonunitspec.UnitSlice().Render()
}
