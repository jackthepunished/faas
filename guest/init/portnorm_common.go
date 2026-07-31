// Port normalization ladder types + pure helpers (ADR-051 §"Consequences").
// The first cold boot of a new deployment may bind any port the customer
// picked — compose-style 3000, Rails-style 5000, Go-static-binary 8001.
// The guest's job is to surface that bind on :8080 so the host's
// `waitReady` can keep dialing 8080 forever and ADR-009's identical-
// inner-network-world invariant stays intact (the vmmd-side waitReady
// is not changed — it always dials 8080).
//
// The runtime install/forward helpers (iptables, splice forwarder) live
// in portnorm_linux.go (`//go:build linux`) — they need AF_INET,
// CAP_NET_ADMIN, and a real kernel. The type + the rung-selection
// helper live here so unit tests on every platform can pin the ladder
// contract.
package main

import "github.com/onebox-faas/faas/pkg/api"

// PortNormMode is the rung of the ladder that activated. Stable
// string values — the host's metric label maps 1:1.
type PortNormMode string

const (
	PortNormNone    PortNormMode = "none"
	PortNormDNAT    PortNormMode = "dnat"
	PortNormForward PortNormMode = "forward"
)

// choosePortNormMode picks the ladder rung. Manifest Port==0 means
// "use DefaultAppPort" — and DefaultAppPort is 8080 — so the app's
// manifest says :8080 is the expected bind. If the app actually
// binds 8080 → mode = none (manifest already aligns). Else: try
// DNAT first, forward as the last resort.
func choosePortNormMode(m api.AppManifest, observed int) PortNormMode {
	want := m.EffectivePort()
	if want == observed {
		return PortNormNone
	}
	return PortNormDNAT // engine probe retries Forward on dnat install fail
}
