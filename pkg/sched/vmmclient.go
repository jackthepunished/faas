// vmmclient.go — schedd's typed wrapper over the generated vmmd gRPC client
// (ADR-014 names this "pkg/sched grpcclient that wraps a vmmd connection").
// schedd is the caller that resolves an app into vmmd's flat AppSpec and drives
// the microVM lifecycle; vmmd stays stateless about app config.
//
// The wrapper does two jobs the raw generated client doesn't:
//   - hides vmmdpb from the rest of pkg/sched (callers pass plain Go structs);
//   - re-lifts vmmd's gRPC error envelope back into *api.Problem via
//     pkg/grpcerr so a wake denial keeps its stable RFC 7807 code all the way
//     out to the gateway (ADR-013).

package sched

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/netip"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/grpcerr"
	"github.com/onebox-faas/faas/pkg/overlay"
	"google.golang.org/grpc"
)

// VMM is the slice of vmmd schedd depends on. Defined as an interface so the
// engine (engine.go, PR2) and its tests can substitute a fake vmmd without a
// real socket — mirrors pkg/vmmdgrpc.VmmdAPI on the server side.
type VMM interface {
	CreateColdBoot(ctx context.Context, instance string, app AppSpec) (*WakeOutcome, error)
	CreateFromSnapshot(ctx context.Context, instance string, app AppSpec, snap SnapshotRef) (*WakeOutcome, error)
	// PauseAndSnapshot (issue #121 / ADR-025 axis 2 slice 4) takes
	// the vmstate_storage_key as a third string alongside vmstatePath
	// and storageKey. The empty string means "single-box default-local
	// uses the legacy host vmstate_path"; a populated value means
	// "vmmd publishes via the configured StorageBackend".
	PauseAndSnapshot(ctx context.Context, instance, vmstatePath, storageKey, vmstateStorageKey string) (SnapshotBytes, error)
	Destroy(ctx context.Context, instance string) error
	// Ping is the wire-level liveness probe (issue #97 / ADR-025
	// axis 3, PR #114). schedd's heartbeat loop calls this every
	// HeartbeatInterval on every active compute_node; a non-error
	// round-trip proves both gRPC socket reachability and that
	// vmmd is responsive enough to schedule the handler. The
	// returned FcVersion lets schedd's admin surface show
	// per-node FC versions without a separate Stats call.
	Ping(ctx context.Context) (*PingOutcome, error)
	// Stats (issue #170 / PR-A, observability slice) returns the
	// per-instance liveness + cgroup view that the schedd poller
	// feeds into pkg/sched/instancestats. Optional pointers
	// (ResidentBytes, CPUPct) decode the proto wrapper types —
	// nil means "no data this sample", which the reader maps to
	// Unknown (see docs/adr/032-instance-metrics-cardinality-rollups.md).
	Stats(ctx context.Context) (*StatsSnapshot, error)
	// Close releases the underlying transport. Issue #120: the
	// heartbeat goroutine dials fresh per tick and relies on this
	// to keep its conn churn bounded (no goroutine/conn leak).
	Close() error
	// UpdateEgressAllowlist (ADR-031 + ADR-033, tier-2 PR-B) pushes
	// a fresh per-app egress allowlist into vmmd's live-instance
	// map without tearing the netns down. RoutedVMM routes by
	// nodeID; the per-node VMMClient forwards over gRPC via the
	// vmmdpb.UpdateEgressAllowlist RPC. vmmd is idempotent (set-
	// equal allowlist is a no-op) so a redelivered event is
	// safe. Errors surface as the gRPC status (Unavailable /
	// Internal) — the egress_drift subscriber logs and drops.
	UpdateEgressAllowlist(ctx context.Context, appID string, allowlist []netip.Prefix) error
	// Logs (issue #254 / Move 4) opens a server-streaming handle
	// on the per-instance ring buffer at vmmd. The returned
	// LogStream is the typed view of vmmdpb.Vmmd_LogsClient; the
	// caller drives it in a loop until EOF (clean) or error.
	//
	// since_seq is the per-instance replay cursor (≥0). 0 = tail
	// from now; pass the last-seen seq+1 to resume after a
	// reconnect. Negative values are clamped to 0 — vmmd's
	// Snapshot filters by Seq >= sinceSeq, so a negative cursor
	// is meaningless and the dial site normalises it.
	//
	// The returned LogStream is alive until the caller closes it
	// via the gRPC closer or the context cancels. RoutedVMM routes
	// by nodeID; the per-node VMMClient forwards over gRPC via
	// the vmmdpb.Logs RPC. vmmd returns codes.NotFound when the
	// instance is not live on this node — the caller (apid) maps
	// that to its own 404 problem.
	Logs(ctx context.Context, instance string, sinceSeq int64) (LogStream, error)
}

// LogReceiver is the per-instance callback the vmmd Logs RPC hands
// each frame to. It returns a non-nil error to abort the stream;
// the vmmd handler surfaces that error over the gRPC trailer.
// A nil return tells the stream to keep delivering the next frame.
//
// The callback is invoked synchronously from the vmmdclient's
// reader goroutine; long-running work inside the callback will
// stall backpressure on the matching vmmd Logs stream. The
// production caller (pkg/scheddgrpc.Server.StreamAppLogs) renders
// each frame to a proto and forwards it onto the caller's gRPC
// stream — that work is bounded by the per-frame size.
type LogReceiver func(seq int64, stream, line string, writtenAt time.Time) error

// LogStream is the typed handle on a vmmd Logs RPC. The caller
// drives the stream by calling Recv in a loop until it returns
// io.EOF (the ring closed / the instance died) or a non-EOF error
// (the dial failed, the context was cancelled, etc).
//
// The production schedd-side caller (pkg/scheddgrpc.Server.StreamAppLogs)
// wraps a LogStream in a goroutine per live instance and merges
// the per-instance frames into the caller's gRPC stream.
type LogStream interface {
	// Recv blocks until the next frame is available or the
	// stream ends. Returns io.EOF on a clean shutdown.
	Recv() (LogLine, error)
}

// LogLine is the typed view of one vmmd Logs(frame). Decoupled
// from vmmdpb so the VMM interface + tests don't import the proto.
type LogLine struct {
	Seq       int64
	Stream    string
	Line      string
	WrittenAt time.Time
}

// PingOutcome is the sched-side view of vmmdpb.PingResponse.
// Decoupled from the proto so the engine + heartbeat loop never
// import pkg/api/proto — same shape, plain time.Time for the
// server-stamped timestamp.
type PingOutcome struct {
	FcVersion  string
	ServerTime time.Time
}

// StatsSnapshot is the typed wrapper over vmmdpb.StatsResponse.
// Decoupled from the proto so the schedd-side instancestats
// package never imports pkg/api/proto. Aggregates stay on the
// outer struct (LiveCount, LeasedCount, TotalResidentBytes);
// per-instance rows are in VMInstanceStat.
//
// Issue #170 / PR-A: TotalResidentBytes and each VMInstanceStat's
// ResidentBytes / CPUPct are *int64 / *float64 (not scalars) because
// the wire distinguishes "absent" (first sample, non-Linux,
// transient cgroup miss) from "real 0". The instancestats poller
// treats absent as Unknown and excludes it from the
// {app,node}-rolled-up Prometheus series until two valid samples
// are observed.
type StatsSnapshot struct {
	LiveCount          int32
	LeasedCount        int32
	TotalResidentBytes *int64
	Instances          []VMInstanceStat
	SampledAt          time.Time
}

// VMInstanceStat is the sched-side view of vmmdpb.InstanceStats —
// one per live VM the queried vmmd owns. InflightRequests and
// LastRequestAt are populated by PR-B (the vmmd ActivityTracker +
// stats handler extraction); until that lands the wire carries
// zero / zero-time and the reader falls back to
// state.Instance.LastRequestAt for the durable timestamp.
type VMInstanceStat struct {
	InstanceID    string
	LeaseUID      int32
	HostIP        string
	ResidentBytes *int64
	CPUPct        *float64
	// CPUSeconds is the cumulative CPU-seconds reading from
	// vmmd's cpustats cache (issue #279 / PR-B). nil on the
	// wire when the cache has no baseline for the instance
	// (first sample, regression, or non-Linux host). schedd
	// maps nil → Unknown / NaN on the wire row and
	// instancestats.InstanceStat.
	CPUSeconds *float64
	// CpuThrottledSeconds (issue #301 / ADR-043) is the
	// cumulative CPU-throttled-seconds reading from vmmd's
	// cpustats cache. Same nil-contract as CPUSeconds — nil
	// on the wire when the cache has no baseline for the
	// instance; schedd decodes non-nil into wire.InstanceStatRow.
	// ThrottledUsec and feeds the per-(account_id, app_id)
	// vmmd_cpu_throttle_seconds_total counter via
	// wire.OpsMetrics.ReplaceInstanceStats.
	CpuThrottledSeconds *float64
	// NetTxBytes (ADR-046, step 7) is the per-tick byte
	// delta on root-side vethHost.rx_bytes from vmmd's
	// netstats cache. nil on the wire when the cache has
	// no baseline for the instance (first sample /
	// regression / netstats cache miss); schedd decodes
	// non-nil into instancestats.InstanceStat.TXBytes and
	// stamps TX=Valid. Only meterd's SampleAndRoll reads
	// the row (pkg/meter/sampler.go, PR-2 fold-in).
	NetTxBytes       *int64
	// NetRxBytes (ADR-048) is the per-tick byte delta on
	// root-side vethHost.tx_bytes — mirror of NetTxBytes on
	// the root→guest (= ingress) direction. Same nil-on-wire
	// contract as NetTxBytes; vmmd ships a wrapper only when
	// the netstats.Cache has a Valid reading for both
	// directions. The wire field awaits make proto regen on
	// the vmmd side; today the field stays nil end-to-end
	// (the poller stamps RX=Unknown, the meterd sampler
	// writes 0 to net_rx_bytes — safe under additive-merge).
	NetRxBytes       *int64
	InflightRequests int64
	LastRequestAt    time.Time
}

// AppSpec is the flat set of fields vmmd needs to boot an instance (ADR-014).
// schedd fills it from its Postgres view of the app + deployment.
//
// Issue #96 / ADR-025 axis 2 / PR #116: BaseKey / LayerKey are the
// StorageBackend keys (not host paths) the wake wire carries. vmmd
// resolves them locally via Storage.Get before staging the chroot.
// The local StorageBackend's Get maps keys to the same files the
// legacy BasePath / LayerPath fields used, so single-box behaviour is
// preserved. BasePath / LayerPath were removed cleanly (internal-only
// consumers, no wire-compat shim).
//
// SealedEnv carries the per-key ciphertext rows from `app_secrets` (spec
// §11/G2). schedd is the only writer that can load these rows (apid writes
// intent, schedd reads to drive wakes). Empty slice = no secrets file
// written; vmmd treats nil and empty as equivalent.
//
// APIEnv (issue #395 / ADR-045) carries the per-key plaintext rows from
// `app_envs`. Distinct from SealedEnv because the values are not sealed
// (env vars are non-sensitive runtime config by contract). Empty slice =
// no env.json file written; vmmd treats nil and empty as equivalent.
// Precedence at the guest layer is "secrets > api_env > manifest_env >
// os.environ".
//
// EgressAllowlist (ADR-031) carries the per-app outbound IP allowlist —
// CIDR strings (e.g. "1.2.3.0/24"), parsed upstream by apid on PUT/PATCH
// and re-validated by the apps.egress_allowlist cidr[] CHECK (v4-only).
// Empty slice = no allowlist rule emitted in the per-netns forward chain
// (current behaviour preserved).
type AppSpec struct {
	BaseKey         string // drive0 base rootfs StorageBackend key (e.g. "base/runtime-node22.ext4")
	LayerKey        string // drive1 per-app layer StorageBackend key (e.g. "apps/<slug>/<depID>.ext4")
	VCPUCount       int32  // 2, or 4 for Scale
	MemSizeMiB      int32  // plan RAM; the slice fences at +8 MiB (pkg/api/limits.go)
	EgressMbit      int32  // per-plan tc cap (pkg/api/limits.EgressMbit); 0 = no cap
	SealedEnv       []fcvm.SealedEnvEntry
	APIEnv          []fcvm.APIEnvEntry // issue #395 / ADR-045: plaintext per-app env
	EgressAllowlist []string           // ADR-031 + ADR-032; v4 or v6 CIDRs; empty = no allowlist rule. The renderer partitions by family.
}

// SnapshotRef points at the snapshot to restore from and the Firecracker
// version it was made with (ADR-005 pinning). An empty ref means cold boot.
//
// #96 / ADR-025 axis 2: StorageKey is the canonical storage key the VMM
// pulls the mem blob from (e.g. "snap/<deploymentID>/mem"). vmmd's
// StorageBackend resolves the bytes through the configured driver into a
// tmp staging path before firing the FC restore. MemPath is gone — the
// deprecation window expired with #96 slice 3.
//
// #121 / ADR-025 axis 2 slice 4: VMStateStorageKey is the canonical
// storage key the VMM pulls the vmstate blob from
// (e.g. "snap/<deploymentID>/vmstate"). When non-empty, vmmd's
// StorageBackend resolves the bytes through the configured driver;
// when empty (default-local), the VMM falls back to VMStatePath (the
// legacy host-path branch the engine reconstructs deterministically on
// wake). The two locators are inclusive in principle but the engine
// only populates one for a given wake: empty for default-local, the
// canonical key for remote nodes. Cold-boot-fallback still requires
// StorageKey (mem F-1 contract).
type SnapshotRef struct {
	DeploymentID string
	VMStatePath  string
	FCVersion    string
	StorageKey   string
	// VMStateStorageKey is the canonical StorageBackend key for the
	// vmstate blob (issue #121 / ADR-025 axis 2 slice 4). Empty on
	// default-local; populated on remote compute nodes.
	VMStateStorageKey string
}

// SnapshotBytes is the size accounting returned by PauseAndSnapshot; schedd
// records it on the snapshot row (fleet-size telemetry, spec §12).
type SnapshotBytes struct {
	MemBytes     int64
	VMStateBytes int64
}

// WakeOutcome is the decoded result of a vmmd wake. Method reports what vmmd
// actually did; RequestedMethod is what schedd asked for (a restore that fell
// back to cold boot per ADR-005 reads Method=WAKE_COLD_BOOT here).
type WakeOutcome struct {
	Instance        string
	LeaseUID        int32
	HostIP          string
	Netns           string
	VethHost        string
	VethPeer        string
	Method          vmmdpb.WakeMethod
	RequestedMethod vmmdpb.WakeMethod
}

// VMMClient is the production VMM: a gRPC connection to vmmd's unix socket.
type VMMClient struct {
	conn *grpc.ClientConn
	cli  vmmdpb.VmmdClient
}

// compile-time assertion that the client satisfies the interface the engine
// consumes.
var _ VMM = (*VMMClient)(nil)

// DialVMM opens a lazy gRPC connection to vmmd's unix socket (ADR-015: the
// socket's 0660/group-`faas` DAC is the only auth for v1.0, so the transport is
// insecure credentials over a trusted local socket). The connection dials on
// first RPC; DialVMM never blocks on vmmd being up.
//
// Per-call deadlines (spec §6.1, commit 1) live at the engine call site,
// not in this client. Each vmmd RPC has a different spec budget (5s for
// WAKING, 30s for COLD_BOOTING, 10s for Destroy) and the engine wraps
// the call with the appropriate context.WithTimeout; centralising
// deadlines here would either over-budget (every RPC gets the largest
// budget) or under-budget (every RPC gets the smallest). Leave the
// client transport-only.
//
// Legacy entrypoint kept for source compatibility with existing
// callers and tests; production code should call DialVMMContext so the
// caller's context controls the dial.
func DialVMM(socketPath string) (*VMMClient, error) {
	return DialVMMContext(context.Background(), socketPath, nil)
}

// DialVMMContext opens a lazy gRPC connection to vmmd. tlsCfg is
// required for tcp/dns targets (issue #95); nil tlsCfg is fine for the
// single-box unix default. Issue #120: the dial routes through
// pkg/overlay so the cross-box dial primitive lives in one place.
// wire.DialContext is still the underlying transport; overlay.Dial
// is the per-compute-node wrapper that ADR-025 axis 3 promises.
func DialVMMContext(ctx context.Context, target string, tlsCfg *tls.Config) (*VMMClient, error) {
	if target == "" {
		return nil, errors.New("sched: empty vmmd target")
	}
	conn, err := overlay.Dial(ctx, overlay.New(target), tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("sched: dial vmmd %q: %w", target, err)
	}
	return &VMMClient{conn: conn, cli: vmmdpb.NewVmmdClient(conn)}, nil
}

// NewVMMClient wraps an already-dialed connection (used by bufconn tests).
func NewVMMClient(conn *grpc.ClientConn) *VMMClient {
	return &VMMClient{conn: conn, cli: vmmdpb.NewVmmdClient(conn)}
}

// Close releases the underlying connection.
func (c *VMMClient) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *VMMClient) CreateColdBoot(ctx context.Context, instance string, app AppSpec) (*WakeOutcome, error) {
	resp, err := c.cli.CreateColdBoot(ctx, &vmmdpb.CreateColdBootRequest{
		Instance: instance,
		App:      app.toProto(),
	})
	if err != nil {
		return nil, liftErr(err)
	}
	return outcomeFromProto(resp), nil
}

func (c *VMMClient) CreateFromSnapshot(ctx context.Context, instance string, app AppSpec, snap SnapshotRef) (*WakeOutcome, error) {
	resp, err := c.cli.CreateFromSnapshot(ctx, &vmmdpb.CreateFromSnapshotRequest{
		Instance: instance,
		App:      app.toProto(),
		Snapshot: &vmmdpb.SnapshotRef{
			DeploymentId:      snap.DeploymentID,
			VmstatePath:       snap.VMStatePath,
			FcVersion:         snap.FCVersion,
			StorageKey:        snap.StorageKey,
			VmstateStorageKey: snap.VMStateStorageKey,
		},
	})
	if err != nil {
		return nil, liftErr(err)
	}
	return outcomeFromProto(resp), nil
}

func (c *VMMClient) PauseAndSnapshot(ctx context.Context, instance, vmstatePath, storageKey, vmstateStorageKey string) (SnapshotBytes, error) {
	resp, err := c.cli.PauseAndSnapshot(ctx, &vmmdpb.PauseAndSnapshotRequest{
		Instance:          instance,
		VmstatePath:       vmstatePath,
		StorageKey:        storageKey,
		VmstateStorageKey: vmstateStorageKey,
	})
	if err != nil {
		return SnapshotBytes{}, liftErr(err)
	}
	return SnapshotBytes{MemBytes: resp.GetMemBytes(), VMStateBytes: resp.GetVmstateBytes()}, nil
}

func (c *VMMClient) Destroy(ctx context.Context, instance string) error {
	if _, err := c.cli.Destroy(ctx, &vmmdpb.DestroyRequest{Instance: instance}); err != nil {
		return liftErr(err)
	}
	return nil
}

// UpdateEgressAllowlist implements VMM. The wire is a repeated
// string of CIDR literals; vmmd partitions by family. Empty
// list is valid (clears the per-app rule on vmmd's side, flips
// the chain policy back to accept). The call is best-effort: a
// gRPC Unavailable / Internal surfaces as a typed error; the
// caller (egress_drift subscriber) logs and drops so a single
// bad patch never blocks the loop. Idempotent on the vmmd side
// — redelivered identical allowlist is a no-op (set-equal
// short-circuit).
func (c *VMMClient) UpdateEgressAllowlist(ctx context.Context, appID string, allowlist []netip.Prefix) error {
	// netip.Prefix → wire string round-trip. Use prefix.String()
	// for canonical form (Masked() already applied at parse time
	// upstream in apid's validateUpdateApp, so the renderer's
	// partition by prefix.Addr().Is4() works unchanged). Nil
	// allowlist → nil slice on the wire (vmmd treats nil and
	// empty identically: clear the rule).
	ss := make([]string, 0, len(allowlist))
	for _, p := range allowlist {
		ss = append(ss, p.String())
	}
	if _, err := c.cli.UpdateEgressAllowlist(ctx, &vmmdpb.UpdateEgressAllowlistRequest{
		AppId:           appID,
		EgressAllowlist: ss,
	}); err != nil {
		return liftErr(err)
	}
	return nil
}

// Logs implements VMM (issue #254 / Move 4). The returned LogStream
// is a thin wrapper over vmmdpb.Vmmd_LogsClient that decouples the
// VMM interface from the proto: every caller in pkg/sched works in
// the typed LogLine shape (no proto import).
//
// sinceSeq is clamped to ≥0 here so downstream callers never have
// to repeat the check; vmmd's Snapshot filters by Seq >= sinceSeq
// and a negative cursor is meaningless (it would replay from
// the oldest snapshot).
func (c *VMMClient) Logs(ctx context.Context, instance string, sinceSeq int64) (LogStream, error) {
	if sinceSeq < 0 {
		sinceSeq = 0
	}
	cli, err := c.cli.Logs(ctx, &vmmdpb.LogsRequest{
		Instance: instance,
		SinceSeq: sinceSeq,
	})
	if err != nil {
		return nil, liftErr(err)
	}
	return &grpcLogStream{cli: cli}, nil
}

// grpcLogStream adapts vmmdpb.Vmmd_LogsClient to the pkg/sched LogStream
// interface. The wrapper exists for two reasons:
//   - decouple the VMM interface from the proto package so engine +
//     tests don't import vmmdpb (mirrors the rest of vmmclient.go)
//   - bound the typed LogLine surface to the four fields the
//     schedd-side caller actually needs
type grpcLogStream struct {
	cli vmmdpb.Vmmd_LogsClient
}

// Recv blocks until the next frame is available or the stream ends.
// A clean vmmd-side shutdown (ring closed, ctx cancelled) surfaces
// as io.EOF; any other failure is the gRPC status lifted via
// grpcerr. The proto's timestamp field is decoded back to time.Time
// so callers never see a *timestamppb.Timestamp.
func (s *grpcLogStream) Recv() (LogLine, error) {
	resp, err := s.cli.Recv()
	if err != nil {
		return LogLine{}, err
	}
	line := LogLine{
		Seq:    resp.GetSeq(),
		Stream: resp.GetStream(),
		Line:   resp.GetLine(),
	}
	if t := resp.GetWrittenAt(); t != nil {
		line.WrittenAt = t.AsTime()
	}
	return line, nil
}

// Ping implements VMM. Wire-level liveness probe (issue #97 /
// ADR-025 axis 3, PR #114); see RoutedVMM.Ping for the contract.
func (c *VMMClient) Ping(ctx context.Context) (*PingOutcome, error) {
	resp, err := c.cli.Ping(ctx, &vmmdpb.PingRequest{})
	if err != nil {
		return nil, liftErr(err)
	}
	out := &PingOutcome{FcVersion: resp.GetFcVersion()}
	if t := resp.GetServerTime(); t != nil {
		out.ServerTime = t.AsTime()
	}
	return out, nil
}

// Stats implements VMM (issue #170 / PR-A, observability slice).
// Decodes the proto wrappers for TotalResidentBytes / ResidentBytes /
// CPUPct so the schedd poller can distinguish "absent" from "real
// 0". Empty InstanceStats rows are returned as the empty
// VMInstanceStat{} value (all-zero fields) — the reader filters
// those by InstanceID == "" rather than via a typed wrapper.
func (c *VMMClient) Stats(ctx context.Context) (*StatsSnapshot, error) {
	resp, err := c.cli.Stats(ctx, &vmmdpb.StatsRequest{})
	if err != nil {
		return nil, liftErr(err)
	}
	out := &StatsSnapshot{
		LiveCount:   resp.GetLiveCount(),
		LeasedCount: resp.GetLeasedCount(),
	}
	if v := resp.GetTotalResidentBytes(); v != nil {
		b := v.GetValue()
		out.TotalResidentBytes = &b
	}
	for _, in := range resp.GetInstances() {
		row := vmInstanceStatFromProto(in)
		if row.InstanceID == "" {
			// Defensive: an empty row would silently look like a
			// real instance with empty fields. The proto should not
			// emit those, but if a vmmd regression does, drop it.
			continue
		}
		out.Instances = append(out.Instances, row)
	}
	return out, nil
}

// vmInstanceStatFromProto decodes one vmmdpb.InstanceStats row into
// the typed wrapper. Pointer fields are nil when the proto wrapper
// is absent (the caller maps that to Unknown). InflightRequests and
// LastRequestAt are populated by PR-B; today the wire carries
// zero / zero-time, which the poller treats as "no signal yet"
// and falls back to state.Instance.LastRequestAt.
func vmInstanceStatFromProto(in *vmmdpb.InstanceStats) VMInstanceStat {
	row := VMInstanceStat{
		InstanceID:       in.GetInstance(),
		LeaseUID:         in.GetLeaseUid(),
		HostIP:           in.GetHostIp(),
		InflightRequests: in.GetInflightRequests(),
	}
	if v := in.GetResidentBytes(); v != nil {
		b := v.GetValue()
		row.ResidentBytes = &b
	}
	if v := in.GetCpuPct(); v != nil {
		c := v.GetValue()
		row.CPUPct = &c
	}
	if v := in.GetCpuSeconds(); v != nil {
		s := v.GetValue()
		row.CPUSeconds = &s
	}
	if v := in.GetCpuThrottledSeconds(); v != nil {
		s := v.GetValue()
		row.CpuThrottledSeconds = &s
	}
	if v := in.GetNetTxBytes(); v != nil {
		b := v.GetValue()
		row.NetTxBytes = &b
	}
	if t := in.GetLastRequestAt(); t != nil {
		row.LastRequestAt = t.AsTime()
	}
	return row
}

func (a AppSpec) toProto() *vmmdpb.AppSpec {
	sealed := make([]*vmmdpb.SealedSecret, 0, len(a.SealedEnv))
	for _, e := range a.SealedEnv {
		sealed = append(sealed, &vmmdpb.SealedSecret{
			Key:        e.Key,
			Ciphertext: e.Ciphertext,
		})
	}
	// Issue #395 / ADR-045: plaintext api_env mirror.
	apiEnv := make([]*vmmdpb.APIEnvEntry, 0, len(a.APIEnv))
	for _, e := range a.APIEnv {
		apiEnv = append(apiEnv, &vmmdpb.APIEnvEntry{
			Key:   e.Key,
			Value: e.Value,
		})
	}
	return &vmmdpb.AppSpec{
		BaseKey:         a.BaseKey,
		LayerKey:        a.LayerKey,
		VcpuCount:       a.VCPUCount,
		MemSizeMib:      a.MemSizeMiB,
		EgressMbit:      a.EgressMbit,
		SealedEnv:       sealed,
		ApiEnv:          apiEnv,
		EgressAllowlist: a.EgressAllowlist,
	}
}

func outcomeFromProto(r *vmmdpb.WakeResponse) *WakeOutcome {
	return &WakeOutcome{
		Instance:        r.GetInstance(),
		LeaseUID:        r.GetLeaseUid(),
		HostIP:          r.GetHostIp(),
		Netns:           r.GetNetns(),
		VethHost:        r.GetVethHost(),
		VethPeer:        r.GetVethPeer(),
		Method:          r.GetMethod(),
		RequestedMethod: r.GetRequestedMethod(),
	}
}

// liftErr converts a vmmd gRPC error back into the platform's *api.Problem so
// its stable Code + Limit/Observed survive to the gateway. Errors that aren't
// status-shaped (e.g. a dial failure) pass through unchanged.
func liftErr(err error) error {
	if p, ok := grpcerr.FromStatus(err); ok && p != nil {
		return p
	}
	return err
}
