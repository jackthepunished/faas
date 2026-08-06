package daemonunitspec

import "github.com/onebox-faas/faas/pkg/daemonunit"

// Entry is one row of the daemon registry — the canonical (name, unit,
// restart classification) tuple that the deploy generator emits into
// both the systemd tree and the cd-controlplane workflow's
// daemons.json.
//
// `Critical` controls which list the daemon lands in
// deploy/etc/daemons.json:
//
//   - true  ⇒ critical[]    (cp-cp cd-controlplane's restart loop)
//   - false ⇒ best_effort[] (advisory restart on deploy, not blocking)
//
// Adding a daemon: add UnitXxx() + register it here. `make generate` will
// pick the new file up on next run; the CI `daemonunit-check` job will
// catch a missing registration.
type Entry struct {
	Name      string
	Unit      func() daemonunit.Unit
	Critical  bool
	Lifecycle Lifecycle
}

type Probe string

const (
	ProbeSystemd Probe = "systemd"
	ProbeUnix    Probe = "unix"
	ProbeTCP     Probe = "tcp"
)

type Lifecycle struct {
	After       []string
	Probe       Probe
	ProbeTarget string
}

func ActivationOrder() []string {
	order := make([]string, len(Registry))
	for i, entry := range Registry {
		order[i] = entry.Name
	}
	return order
}

// Registry is the single source of truth for which daemons the platform
// ships. Order in the slice is the order cd-controlplane's restart loop
// runs them (vmmd first because it owns /run/faas; imaged last because
// it is best-effort). The CI gate `daemonunit-check` is order-insensitive
// (Diff matches by set membership) — order here is for human readability
// and the order the workflow actually restarts the services in.
var Registry = []Entry{
	{Name: "vmmd", Unit: UnitVmmd, Critical: true, Lifecycle: Lifecycle{Probe: ProbeUnix, ProbeTarget: "/run/faas/vmmd.sock"}},
	{Name: "apid", Unit: UnitApid, Critical: true, Lifecycle: Lifecycle{Probe: ProbeTCP, ProbeTarget: "127.0.0.1:8081"}},
	{Name: "schedd", Unit: UnitSchedd, Critical: true, Lifecycle: Lifecycle{After: []string{"vmmd"}, Probe: ProbeUnix, ProbeTarget: "/run/faas/schedd.sock"}},
	{Name: "gatewayd-internal", Unit: UnitGatewaydInternal, Critical: true, Lifecycle: Lifecycle{After: []string{"schedd", "apid"}, Probe: ProbeTCP, ProbeTarget: "127.0.0.1:9090"}},
	{Name: "gatewayd-public", Unit: UnitGatewaydPublic, Critical: true, Lifecycle: Lifecycle{After: []string{"gatewayd-internal"}, Probe: ProbeTCP, ProbeTarget: "127.0.0.1:8080"}},
	{Name: "meterd", Unit: UnitMeterd, Critical: true, Lifecycle: Lifecycle{After: []string{"apid"}, Probe: ProbeSystemd}},
	{Name: "githubd", Unit: UnitGithubd, Critical: true, Lifecycle: Lifecycle{After: []string{"apid"}, Probe: ProbeSystemd}},
	{Name: "imaged", Unit: UnitImaged, Critical: false, Lifecycle: Lifecycle{After: []string{"vmmd"}, Probe: ProbeSystemd}},
}

// FaasCPSlice is the [Slice] MemoryMax=3G ceiling for the entire
// control-plane slice. The 3 GB is hardcoded here (not derived from
// the financial model §13 line 431 — the model says 6 GB but the
// shipped slice is 3 GB; tracked as a known under-utilisation that
// can be widened in a future PR when the daemon set + memory profile
// stabilises post-DEPLOY-1).
//
// The slice is emitted to deploy/controlplane/systemd/faas-cp.slice
// only; it is NOT a daemon and lives outside the Registry iteration
// because it is the wrapper, not a member.
const FaasCPSlice = "faas-cp.slice"
