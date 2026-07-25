// Package vmmdgrpc turns pkg/fcvm.Manager into the gRPC service defined in
// api/proto/onebox/faas/vmmd/v1. Handlers stay thin — each one wraps a single
// Manager call and translates its result into the proto envelope. The wire
// shape is fixed by ADR-013/014/016; this file does not invent fields.
//
// Every handler ≤ 50 lines per spec §Conventions line 472. Anything bigger
// gets extracted to proto.go (type adapters) or stats.go (Stats workload).

package vmmdgrpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/fcvm/leakcheck"
	"github.com/onebox-faas/faas/pkg/grpcerr"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// VmmdAPI is the slice of pkg/fcvm.Manager that the handlers need. Defined
// here (not imported) so the unit tests can pass a fake without depending
// on the firecracker side effects.
type VmmdAPI interface {
	Wake(ctx context.Context, req fcvm.WakeRequest) (*fcvm.Instance, error)
	Park(ctx context.Context, instance string, spec fcvm.SnapshotSpec) (fcvm.SnapshotInfo, error)
	Destroy(ctx context.Context, instance string) error
	DestroyWithExport(ctx context.Context, instance, exportDir string) (int, error)
	LiveCount() int
	LeasedCount() int
	// NetnsFor is the issue #98 / ADR-028 bridge: the ForwardHTTP
	// handler needs the per-instance netns name (fc-<instance>) to
	// nsenter before dialing netns.GuestIP:netns.AppPort. Defined here
	// (not in pkg/vmmdgrpc/forward.go) so the Server struct's
	// interface check catches a Manager wiring gap at compile time.
	NetnsFor(instance string) (string, bool)
	// UpdateEgressAllowlist (ADR-031 + ADR-033, tier-2 PR-B) walks
	// the live-instance map and applies the new per-app egress
	// allowlist in-place via incremental nft patch. Idempotent: an
	// empty allowlist flips the chain policy back to accept; an
	// allowlist equal to the cached prior one is a no-op. schedd
	// invokes this from the egress-drift subscriber
	// (pkg/sched/egress_drift.go) on every pg_notify app_changed
	// payload with kind="updated".
	UpdateEgressAllowlist(ctx context.Context, appID string, allowlist []netip.Prefix) error
	// InstancePID (M8 §11 — seccomp assertion) returns the host PID
	// of the running jailer child for instance, or (0, false) if
	// the instance is not currently alive. The SeccompStatus gRPC
	// handler reads /proc/<pid>/status to verify the jailer default
	// seccomp filter is in place. Defined on VmmdAPI (not on
	// pkg/fcvm.Manager) so the handler reads through the narrow
	// seam the Manager already exposes — no new Manager method.
	InstancePID(instance string) (int, bool)
}

// Server implements vmmdpb.VmmdServer.
type Server struct {
	vmmdpb.UnimplementedVmmdServer

	vmm   VmmdAPI
	ops   *wire.OpsMetrics
	fcVer string
	log   *slog.Logger
}

// New wires the server. ops may be nil (noop metrics), log may be nil
// (slog default).
func New(vmm VmmdAPI, ops *wire.OpsMetrics, fcVer string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		// Use a fresh registry with a no-op prefix; observe still records but
		// never exported. Tests that don't assert metrics use this path.
		ops = wire.NewOpsMetrics("vmmd_test")
	}
	return &Server{vmm: vmm, ops: ops, fcVer: fcVer, log: log}
}

// Register binds s to a gRPC server.
func (s *Server) Register(g *grpc.Server) {
	vmmdpb.RegisterVmmdServer(g, s)
}

// CreateFromSnapshot wires the snapshot-restore path. Falls back to cold
// boot inside Manager.Wake — the response's `method` reports what
// actually happened. ADR-005 is enforced one layer down.
func (s *Server) CreateFromSnapshot(ctx context.Context, req *vmmdpb.CreateFromSnapshotRequest) (*vmmdpb.WakeResponse, error) {
	const op = "CreateFromSnapshot"
	start := time.Now()
	wr, err := toWakeRequest(req)
	if err != nil {
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	inst, err := s.vmm.Wake(ctx, wr)
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return wakeResponseFromInstance(req.GetInstance(), wr, inst, vmmdpb.WakeMethod_WAKE_RESTORE), nil
}

// CreateColdBoot primes an instance for the deploy-pipeline first-boot
// path (no snapshot).
func (s *Server) CreateColdBoot(ctx context.Context, req *vmmdpb.CreateColdBootRequest) (*vmmdpb.WakeResponse, error) {
	const op = "CreateColdBoot"
	start := time.Now()
	wr, err := toColdBootRequest(req)
	if err != nil {
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	inst, err := s.vmm.Wake(ctx, wr)
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		s.log.Error("vmmd: cold boot failed", "instance", req.GetInstance(), "err", err.Error())
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return wakeResponseFromInstance(req.GetInstance(), wr, inst, vmmdpb.WakeMethod_WAKE_COLD_BOOT), nil
}

// PauseAndSnapshot parks an instance, writing its full snapshot. Destroy
// happens inside Manager.Park.
//
// #96 / ADR-025 axis 2: mem + vmstate are both routed through the
// configured StorageBackend. vmmd allocates staging tmps internally
// (SnapshotSpec.StageMemPath), the mem blob is published at the
// canonical StorageKey, and the vmstate blob is published at the
// canonical VMStateStorageKey when one is supplied. Either vmstate
// locator (legacy VMStatePath or new VMStateStorageKey) is acceptable
// to keep default-local single-box behaviour bit-for-bit: the engine
// (pkg/sched/engine.go) sends the empty key for default-local so the
// legacy host-path branch is taken. The two locators are NOT mutually
// exclusive — a remote caller may populate both and the storage key
// is authoritative; VmstatePath is logged-only metadata when the
// storage key is non-empty.
func (s *Server) PauseAndSnapshot(ctx context.Context, req *vmmdpb.PauseAndSnapshotRequest) (*vmmdpb.SnapshotResponse, error) {
	const op = "PauseAndSnapshot"
	start := time.Now()
	// Mem storage_key stays required (F-1 contract, #96 slice 3). Vmstate
	// is acceptable via either vmstate_storage_key (new, ADR-025 axis 2
	// slice 4) or vmstate_path (legacy host path, single-box default).
	// Neither vmstate field set together is rejected so an operator who
	// forgets both gets a clear error naming both field names.
	if req.GetStorageKey() == "" || (req.GetVmstateStorageKey() == "" && req.GetVmstatePath() == "") {
		err := api.NewProblem(int(codes.InvalidArgument), api.CodeValidation,
			"Missing paths",
			"storage_key is required; at least one of vmstate_storage_key or vmstate_path must be set").
			WithDocs("https://docs/DOMAIN/vmmd#pause")
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(err)
	}
	info, err := s.vmm.Park(ctx, req.GetInstance(), fcvm.SnapshotSpec{
		VMStatePath:       req.GetVmstatePath(),
		StorageKey:        req.GetStorageKey(),
		VMStateStorageKey: req.GetVmstateStorageKey(),
	})
	s.ops.Observe(op, time.Since(start), err)
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return &vmmdpb.SnapshotResponse{
		MemBytes:     info.MemBytes,
		VmstateBytes: info.VMStateBytes,
	}, nil
}

// Destroy tears down an instance. Idempotent for unknown instances. The
// optional ExportDir (passed via CreateColdBoot.BuildSpec and remembered by
// vmmd) triggers a build-aware teardown: vmmd waits for the in-VM build to
// exit, captures the exit code, and copies /build/out/* + build-done.json
// into ExportDir before releasing the chroot. The response carries the exit
// code on the wire so builderd can classify (FailureUserError / OOM / Timeout).
func (s *Server) Destroy(ctx context.Context, req *vmmdpb.DestroyRequest) (*vmmdpb.DestroyResponse, error) {
	const op = "Destroy"
	start := time.Now()
	exportDir := s.exportDirFor(req.GetInstance())
	code, err := s.vmm.DestroyWithExport(ctx, req.GetInstance(), exportDir)
	if err != nil {
		s.ops.Observe(op, time.Since(start), err)
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.DestroyResponse{Instance: req.GetInstance(), ExitCode: int32(code)}, nil
}

// exportDirFor asks the Manager whether the instance was registered as a
// builder VM at cold-boot. App VMs return "" (so the gRPC Destroy stays
// backwards-compatible — same teardown behaviour as before M6).
func (s *Server) exportDirFor(instance string) string {
	if getter, ok := s.vmm.(interface {
		ExportDirFor(string) string
	}); ok {
		return getter.ExportDirFor(instance)
	}
	return ""
}

// Heartbeat answers the presence ping from schedd's heartbeat
// goroutine (issue #98 / ADR-028). The direction is reversed from
// the vmmd-pushes design because schedd is the admission authority
// and shouldn't trust inbound traffic from a box it may have already
// drained — the heartbeat lives on the schedd side and just proves
// "this vmmd is reachable over the overlay right now". Returning a
// non-Unavailable gRPC code is what schedd counts as liveness.
func (s *Server) Heartbeat(ctx context.Context, _ *vmmdpb.HeartbeatRequest) (*vmmdpb.HeartbeatResponse, error) {
	const op = "Heartbeat"
	start := time.Now()
	s.ops.Observe(op, time.Since(start), nil)
	return &vmmdpb.HeartbeatResponse{}, nil
}

// Stats returns Manager's view: live/leased counts and per-instance
// resident bytes sourced from cgroup memory.current.
func (s *Server) Stats(ctx context.Context, _ *vmmdpb.StatsRequest) (*vmmdpb.StatsResponse, error) {
	const op = "Stats"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()

	resp := &vmmdpb.StatsResponse{
		LiveCount:   int32(s.vmm.LiveCount()),
		LeasedCount: int32(s.vmm.LeasedCount()),
	}

	resident, ok := leakcheck.ResidentBytes()
	if !ok {
		// Non-Linux host (dev). Unset rather than zero — DistinctValue
		// semantics let dashboards distinguish "no data" from "0".
		resp.TotalResidentBytes = nil
		return resp, nil
	}

	var total int64
	resp.Instances = make([]*vmmdpb.InstanceStats, 0, len(resident))
	for inst, b := range resident {
		total += b
		resp.Instances = append(resp.Instances, &vmmdpb.InstanceStats{
			Instance:      inst,
			ResidentBytes: wrapperspb.Int64(b),
		})
	}
	resp.TotalResidentBytes = wrapperspb.Int64(total)
	return resp, nil
}

// Ping is a wire-only liveness probe (issue #97 / ADR-025 axis 3,
// PR #114). Returns the vmmd process's Firecracker version + the
// server-side timestamp at the moment the handler ran. schedd's
// heartbeat loop calls this on every active compute_node every
// HeartbeatInterval (default 30s); a successful round-trip proves
// both gRPC socket reachability and that vmmd's goroutine
// scheduler is responsive enough to schedule this handler. Idempotent
// + side-effect free; no backing Manager call.
func (s *Server) Ping(_ context.Context, _ *vmmdpb.PingRequest) (*vmmdpb.PingResponse, error) {
	const op = "Ping"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()
	return &vmmdpb.PingResponse{
		FcVersion:  s.fcVer,
		ServerTime: timestamppb.Now(),
	}, nil
}

// UpdateEgressAllowlist (ADR-031 + ADR-033, tier-2 PR-B) walks the
// vmmd's live-instance map and applies the new per-app egress
// allowlist in-place via incremental nft patch (Manager.UpdateEgressAllowlist).
// Empty allowlist = clear the rule and let chain-policy default
// accept do its work. Idempotent on a redelivered identical
// allowlist (no-op). Schedd invokes this from the egress-drift
// subscriber on every pg_notify app_changed payload with
// kind="updated"; failures bubble up as Unavailable so schedd
// logs + retries on its next reconcile.
func (s *Server) UpdateEgressAllowlist(ctx context.Context, req *vmmdpb.UpdateEgressAllowlistRequest) (*vmmdpb.UpdateEgressAllowlistAck, error) {
	const op = "UpdateEgressAllowlist"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()
	if req.GetAppId() == "" {
		return nil, grpcerr.ToStatus(toProblem(api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing app_id", "app_id is required").
			WithDocs("https://docs/DOMAIN/vmmd#update-egress-allowlist")))
	}
	allowlist, err := toEgressAllowlist(req.GetEgressAllowlist())
	if err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	if err := s.vmm.UpdateEgressAllowlist(ctx, req.GetAppId(), allowlist); err != nil {
		return nil, grpcerr.ToStatus(toProblem(err))
	}
	return &vmmdpb.UpdateEgressAllowlistAck{}, nil
}

// SeccompStatus (M8 §11) reports the kernel seccomp state of the
// jailer child backing instance. Sequence:
//  1. Resolve the running jailer PID via VmmdAPI.InstancePID.
//     Unknown instance → NotFound (the e2e tripwire wants "no
//     instance" to be visible on the wire, not silently absorbed).
//  2. Read /proc/<pid>/status from the vmmd process (the test
//     reads again from the e2e process; both should agree because
//     they read the same kernel state — the tripwire is about the
//     value, not the reader).
//  3. Parse the `Seccomp:` and `Seccomp_filters:` lines (kernel
//     text format; the kernel writes them in this order). Map
//     0/1/2 to "disabled"/"strict"/"filter" (the kernel's own
//     mode names).
//
// Empty instance is InvalidArgument — distinguishes "operator
// forgot the field" from "operator named a dead instance" so the
// gRPC code surfaces the diagnosis. The handler does not call
// into Manager (seccomp is a per-process kernel attribute, not a
// Manager concern); the wire is the only contract.
func (s *Server) SeccompStatus(ctx context.Context, req *vmmdpb.SeccompStatusRequest) (*vmmdpb.SeccompStatusResponse, error) {
	const op = "SeccompStatus"
	start := time.Now()
	defer func() { s.ops.Observe(op, time.Since(start), nil) }()

	if req.GetInstance() == "" {
		return nil, grpcerr.ToStatus(api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing instance", "instance is required").
			WithDocs("https://docs/DOMAIN/vmmd#seccomp"))
	}
	pid, ok := s.vmm.InstancePID(req.GetInstance())
	if !ok {
		return nil, grpcerr.ToStatus(api.NewProblem(int(codes.NotFound),
			api.CodeNotFound, "Instance not alive", fmt.Sprintf("instance %q is not alive on this vmmd", req.GetInstance())).
			WithDocs("https://docs/DOMAIN/vmmd#seccomp"))
	}

	mode, filterLen, err := readSeccompStatus(pid)
	if err != nil {
		// Don't fail the gRPC call with Internal — the e2e needs
		// the wire to be OK so the response carries the
		// diagnostic message. The presence of Error is the
		// failure signal the test asserts on.
		return &vmmdpb.SeccompStatusResponse{
			Instance: req.GetInstance(),
			Pid:      int32(pid),
			Mode:     "unknown",
			Error:    err.Error(),
		}, nil
	}
	return &vmmdpb.SeccompStatusResponse{
		Instance:  req.GetInstance(),
		Pid:       int32(pid),
		Mode:      mode,
		FilterLen: int32(filterLen),
	}, nil
}

// readSeccompStatus parses /proc/<pid>/status and returns (mode, filterLen, err).
// Mode is the human-readable equivalent of the kernel's Seccomp integer
// ("disabled" / "strict" / "filter"). filterLen is the number of distinct
// BPF filter programs attached to the process (the `Seccomp_filters:` line).
// An error means the file couldn't be read or the kernel didn't write the
// expected lines — the handler maps that to Error="…" in the response.
func readSeccompStatus(pid int) (string, int, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", 0, fmt.Errorf("open /proc/%d/status: %w", pid, err)
	}
	defer f.Close()
	mode, filterLen, err := parseSeccompLines(f)
	if err != nil {
		return "", 0, fmt.Errorf("parse /proc/%d/status: %w", pid, err)
	}
	return mode, filterLen, nil
}

// parseSeccompLines is the Read-from-text twin of readSeccompStatus.
// Exposed so the in-process test (seccomp_test.go) can pin the
// kernel-ABI parser without spinning up a real /proc/<pid>/status.
// The two functions MUST stay in sync — if they diverge, the
// cross-process e2e catches the drift.
func parseSeccompLines(r io.Reader) (string, int, error) {
	var mode string
	var filterLen int
	haveMode, haveFilter := false, false
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Seccomp:"):
			// Line format: "Seccomp:         2" (kernel pads with spaces).
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return "", 0, fmt.Errorf("malformed Seccomp line: %q", line)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return "", 0, fmt.Errorf("parse Seccomp value %q: %w", fields[1], err)
			}
			switch n {
			case 0:
				mode = "disabled"
			case 1:
				mode = "strict"
			case 2:
				mode = "filter"
			default:
				return "", 0, fmt.Errorf("unknown Seccomp value %d", n)
			}
			haveMode = true
		case strings.HasPrefix(line, "Seccomp_filters:"):
			// Line format: "Seccomp_filters: 1" (kernel writes this
			// only when Seccomp is 2; missing line is fine if the
			// mode is 0/1).
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return "", 0, fmt.Errorf("malformed Seccomp_filters line: %q", line)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return "", 0, fmt.Errorf("parse Seccomp_filters value %q: %w", fields[1], err)
			}
			filterLen = n
			haveFilter = true
		}
		if haveMode && haveFilter {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return "", 0, fmt.Errorf("scan: %w", err)
	}
	if !haveMode {
		return "", 0, fmt.Errorf("no Seccomp line (kernel too old?)")
	}
	return mode, filterLen, nil
}

// toProblem lifts a plain error to *api.Problem if it isn't one already.
// Manager errors are *fmt.Errorf-wrapped strings, so we synthesise an
// Internal problem rather than risk leaking go-internals across the wire.
func toProblem(err error) *api.Problem {
	if err == nil {
		return nil
	}
	if p := api.AsProblem(err); p != nil {
		return p
	}
	return api.NewProblem(int(codes.Internal), "internal",
		"vmmd operation failed", err.Error())
}

// unused import guard.
var _ = fmt.Sprintf
