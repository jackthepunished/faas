package fcvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"filippo.io/age"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/logsanitize"
	"github.com/onebox-faas/faas/pkg/netns"
	"github.com/onebox-faas/faas/pkg/secretbox"
)

// Manager is vmmd's core: it owns the whole per-instance resource lifecycle —
// lease → network → jailed firecracker → teardown. Its central guarantee is that
// EVERY failure path fully unwinds (network torn down, VM killed, lease released),
// so the box never leaks netns/TAPs/uids/cgroups (invariant §6.2-4/5,
// `make leakcheck`). The side effects are injected so this guarantee is proven by
// unit tests without KVM; the metal implementations live behind //go:build metal.

// Runner executes one host command (ip/nft/sysctl) to completion.
type Runner interface {
	Run(ctx context.Context, argv []string) error
}

// VMM starts, snapshots, restores, and stops the jailed firecracker process for
// an instance.
type VMM interface {
	// Boot spawns jailer→firecracker with cfg and returns once the guest passes
	// readiness. It must clean up its own chroot/process if it returns an error.
	Boot(ctx context.Context, l Lease, cfg VMConfig) error
	// BootColdBoot is the cold-boot entry point (issue #96 / ADR-025 axis 2
	// / PR #116): it materializes the StorageBackend keys in spec through
	// the configured backend into vmmd-allocated tmp paths, then delegates
	// to Boot. Manager.Wake prefers BootColdBoot over Boot; tests that
	// already have resolved paths in hand can keep using Boot directly.
	BootColdBoot(ctx context.Context, l Lease, spec ColdBootSpec) error
	// Restore loads a snapshot into a fresh jailed firecracker and resumes it,
	// returning once the guest is ready. On error it cleans up its own process.
	Restore(ctx context.Context, l Lease, spec RestoreSpec) error
	// TriggerResumeHook dials the guest's vsock UDS and asks it to run its
	// post-restore side effects (re-seed entropy + step clock, guest/init/resume.go).
	// Must be called from Restore after /snapshot/load and before waitReady so
	// the app cannot accept on :8080 with a stale RNG stream (spec §11 V6).
	// ADR-022 records the wire format (4-byte msg type + JSON body, port 1024
	// on the fixed host CID 3).
	TriggerResumeHook(ctx context.Context, l Lease, hostTimeUnixNano int64) error
	// Snapshot pauses the running VM, writes a full snapshot to spec's paths, and
	// destroys the VM (spec §4.4). The instance is gone when this returns.
	Snapshot(ctx context.Context, l Lease, spec SnapshotSpec) (SnapshotInfo, error)
	// Kill stops the firecracker process and removes the jail chroot. It is
	// best-effort and idempotent — safe to call on an instance that never fully
	// booted.
	Kill(ctx context.Context, l Lease) error
	// DestroyWithExport is the build-aware teardown: it waits for the firecracker
	// child to exit, captures the exit code, and (if exportDir != "") loopback-
	// mounts the chroot-local drive1 to copy out the produced artifacts before
	// removing the chroot. App VMs pass exportDir=""; builder VMs (M6) pass the
	// host directory builderd wants files under. Returns the captured exit code
	// (0 for app VMs, the build's own exit code for builder VMs).
	DestroyWithExport(ctx context.Context, l Lease, exportDir string) (int, error)
	// StageSecretsEnv is the G2 write-side counterpart to DestroyWithExport's
	// read-side artifact pull: loopback-mounts drive1 in the chroot, writes
	// /etc/faas/secrets.env (already-unsealed JSON), and umounts. jsonBlob may
	// be empty — implementations MUST treat that as a no-op so apps without
	// secrets skip the mount/umount cycle entirely.
	StageSecretsEnv(instance string, jsonBlob []byte) error
}

// Paths locates the kernel and base images on disk (spec §8). Injected so tests
// don't touch the filesystem.
type Paths struct {
	Kernel string // /srv/fc/base/vmlinux-6.1.x
}

// Instance is a live (or booting) microVM tracked by the Manager.
type Instance struct {
	Lease  Lease
	Net    netns.Config
	Method WakeMethod // how it came up; a restore that fell back reads WakeColdBoot
	// AppID is the apps.id UUID the instance was woken for.
	// UpdateEgressAllowlist (PR-B, ADR-031+033) uses it to walk
	// the live map keyed by app instead of by instance, so a
	// single PATCH on apps.egress_allowlist patches every live
	// instance of the app without the caller enumerating them.
	// Stored on the instance (not the Lease) so the Lease
	// stays allocator-owned and the Instance carries the
	// schedd-owned app identity.
	AppID string

	// AllowlistHandleV4 / V6 are the nft handles of the
	// per-netns allowlist accept rules captured at Wake time (or
	// at the previous successful UpdateEgressAllowlist). Used by
	// UpdateEgressAllowlist to delete the prior rule by handle
	// before inserting the new one — the in-place patch that
	// keeps the live netns in sync without a cold-wake. Zero
	// when the family half is empty (no rule was emitted at
	// Wake / patch time). The handle is captured by re-listing
	// the chain with `nft -a list chain` after the rule is
	// inserted; the metal test exercises this code path; the
	// unit suite stubs it out.
	AllowlistHandleV4 uint64
	AllowlistHandleV6 uint64
}

// Manager tracks live instances and serialises nothing on the hot path beyond a
// short-held map lock. Safe for concurrent Wake/Destroy.
type Manager struct {
	alloc *Allocator
	run   Runner
	// captureRunner (tier-2 PR-B) is the optional stdout-aware
	// handle used by captureAllowlistHandles to read `nft -a list
	// chain` output and resolve the freshly-added allowlist
	// rule's kernel-assigned handle. nil means "no capture
	// available"; the wake path then leaves AllowlistHandle{V4,V6}
	// at 0, and UpdateEgressAllowlist will add the new rule
	// alongside the prior one (still correct, just leaves an
	// orphan until the next patch picks it up via the chain
	// list). Production wires the metal runner that wraps
	// exec.CommandContext with CombinedOutput; unit tests can
	// stub via WithCaptureRunner.
	captureRunner CaptureRunner
	vmm           VMM
	paths         Paths
	fcVersion     string // the running Firecracker version; snapshots load only on a match
	log           *slog.Logger

	mu         sync.Mutex
	live       map[string]*Instance
	exportDirs map[string]string // instance -> host export dir (builder VMs only, M6)
	// metrics is the cold-boot fallback counter (vmmd_cold_boot_fallback_total).
	// nil-safe: bringUp calls m.metrics.ObserveFallback() which no-ops when nil,
	// so unit tests that construct a Manager without metrics don't need a stub.
	metrics *ColdBootMetrics
	// hostIdentity is the X25519 secret key used to unseal per-app sealed env
	// blobs at wake time (spec §11/G2). nil means "no host age configured" —
	// a Wake call with SealedEnvEntries set is rejected with ErrNoHostKey
	// rather than silently dropping plaintext. vmmd owns the on-disk file.
	hostIdentity *age.X25519Identity
	// conntrackCap is the effective per-instance conntrack cap. Probed once
	// at construction from api.ConntrackCapProbe(): DefaultConntrackCap when
	// the kernel supports ct expressions in netns (CONFIG_NF_CONNTRACK_NET_NS),
	// 0 when it doesn't (the ct cap rule is omitted, egress tc cap unaffected).
	conntrackCap int64
}

// NewManager wires a Manager. fcVersion is the running Firecracker version (used
// to decide snapshot usability, ADR-005). log may be nil (a discard logger).
// metrics may be nil (e.g. unit tests that don't care about Prometheus); the
// fallback counter is then a no-op (ColdBootMetrics.ObserveFallback is nil-safe).
func NewManager(run Runner, vmm VMM, paths Paths, fcVersion string, log *slog.Logger, metrics *ColdBootMetrics) *Manager {
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Manager{
		alloc:        NewAllocator(),
		run:          run,
		vmm:          vmm,
		paths:        paths,
		fcVersion:    fcVersion,
		log:          log,
		live:         make(map[string]*Instance),
		exportDirs:   make(map[string]string),
		metrics:      metrics,
		conntrackCap: api.ConntrackCapProbe(),
	}
}

// SetHostIdentity attaches the unseal key. Only vmmd calls this — the
// Manager holds the private half for the duration of the process. NOT
// safe to call concurrently with Wake; production wires it before
// serving traffic.
func (m *Manager) SetHostIdentity(id *age.X25519Identity) {
	m.hostIdentity = id
}

// HostIdentity returns the identity the Manager was constructed with
// (nil if SetHostIdentity was never called). Used by tests and by the
// daemon's start-up self-check.
func (m *Manager) HostIdentity() *age.X25519Identity { return m.hostIdentity }

// ErrNoHostKey is returned when a WakeRequest carries SealedEnvEntries
// but the Manager was not configured with a host identity. Surface this
// to schedd so the wake fails fast — never silently drop the ciphertext
// or accept-and-discard the plaintext.
var ErrNoHostKey = errors.New("fcvm: host identity not loaded")

// jsonMarshalEnvelope re-marshals the unsealed Envelope to canonical JSON.
// Lives in manager.go (not secretbox) because it's part of the staging
// step, not the seal/open API surface.
func jsonMarshalEnvelope(e secretbox.Envelope) ([]byte, error) {
	return json.Marshal(e)
}

// stageSecretsEnv delegates to the VMM's loopback-mount write. The Manager
// holds no mount logic of its own — the VMM owns the chroot root + instance
// layout (JailerVMM) or, in tests, a stub that writes the file directly.
func (m *Manager) stageSecretsEnv(instance string, jsonBlob []byte) error {
	return m.vmm.StageSecretsEnv(instance, jsonBlob)
}

// WakeRequest brings an app up for a request or cron (spec §6.1). If Snapshot is
// usable on the running Firecracker version it is restored (fast path); otherwise,
// or if restore fails, the instance cold boots from rootfs (ADR-005: cold boot
// always works). BaseKey/LayerKey are required for the cold path.
//
// Issue #96 / ADR-025 axis 2 (PR #116): BaseKey / LayerKey are the
// StorageBackend keys schedd sends on the wake wire (not host paths).
// vmmd resolves them via Storage.Get before staging the chroot. The
// local backend's Get maps the same keys to the same files the legacy
// *Path fields used, so single-box behaviour is preserved. Field
// names changed from *Path → *Key to match the new semantics.
type WakeRequest struct {
	Instance   string
	AppID      string // apps.id UUID; PR-B UpdateEgressAllowlist walks live by AppID
	BaseKey    string // StorageBackend key for drive0 shared ro base rootfs for the app's runtime
	LayerKey   string // StorageBackend key for drive1 per-app layer
	VcpuCount  int
	MemSizeMiB int
	EgressMbit int       // per-plan tc cap (pkg/api/limits.EgressMbit); 0 = no cap
	Snapshot   *Snapshot // nil => cold boot
	// ExportDir, if non-empty, marks this instance as a builder VM (M6).
	// vmmd's Manager.DestroyWithExport waits for exit, captures the exit code,
	// and copies build artifacts (build-done.json + /build/out/*) into this host
	// directory. App VMs leave it empty.
	ExportDir string
	// SealedEnvEntries are the per-key ciphertext rows from `app_secrets`
	// the caller wants loaded into the guest's env (spec §11/G2). Each entry is
	// sealed independently by apid via pkg/secretbox.SealOne against the host
	// X25519 recipient; vmmd unseals each, merges into an envelope, and writes
	// /etc/faas/secrets.env on drive1. Empty slice = no file written.
	//
	// Per-key (rather than one combined envelope) because that's how apid
	// already persists them — the wire stays narrow and unseal work scales with
	// the per-app quota (≤100 keys at Scale), not with arbitrary blob lengths.
	//
	// The plaintext is held ONLY in memory by the manager at this point — the
	// Manager is the unseal-and-forget boundary. It is never logged, never
	// persisted, never returned to any caller.
	SealedEnvEntries []SealedEnvEntry
	// EgressAllowlist (ADR-031, tier-2 of the network roadmap): per-app
	// outbound IPv4 allowlist. Each entry is a CIDR string (e.g.
	// "1.2.3.0/24"); empty slice = current behaviour (no allowlist rule
	// emitted, every non-deny destination is reachable). When non-empty,
	// the per-netns forward chain gains a single
	//   `iifname "tap0" ip daddr { <CIDRs> } accept`
	// rule after the lateral-movement deny + SMTP drops; deny > allow on
	// overlap, so a typoed RFC1918 CIDR still gets dropped. Plan-gated
	// upstream — Free/Hobby never get here; Pro ≤ 16; Scale ≤ 64. The
	// caller (apid) is responsible for size + per-plan gating.
	EgressAllowlist []string
}

// SealedEnvEntry is one (key, ciphertext) pair as stored in app_secrets. The
// key is the env-var name; the ciphertext is sealed under the host age
// recipient by apid. vmmd merges all entries into the single envelope file.
type SealedEnvEntry struct {
	Key        string
	Ciphertext []byte
}

// ColdBootRequest is the deploy-pipeline prime path: a first boot with no
// snapshot yet (spec §9.6).
//
// Issue #96 / ADR-025 axis 2 (PR #116): BaseKey / LayerKey are the
// StorageBackend keys schedd sends on the wake wire (not host paths).
// Same semantics as WakeRequest.
type ColdBootRequest struct {
	Instance   string
	BaseKey    string
	LayerKey   string
	VcpuCount  int
	MemSizeMiB int
	EgressMbit int // per-plan tc cap; 0 = no cap (legacy / disabled)
	// ExportDir is non-empty for builder VMs. See WakeRequest.
	ExportDir string
	// SealedEnvEntries is forwarded to WakeRequest for staging onto drive1
	// (spec §11/G2). Empty slice = no secrets file written.
	SealedEnvEntries []SealedEnvEntry
	// EgressAllowlist (ADR-031) — same shape as WakeRequest.
	EgressAllowlist []string
}

// ColdBoot boots an instance from rootfs with no snapshot. It is Wake with a nil
// snapshot.
func (m *Manager) ColdBoot(ctx context.Context, req ColdBootRequest) (*Instance, error) {
	return m.Wake(ctx, WakeRequest{
		Instance: req.Instance, BaseKey: req.BaseKey, LayerKey: req.LayerKey,
		VcpuCount: req.VcpuCount, MemSizeMiB: req.MemSizeMiB,
		EgressMbit: req.EgressMbit, Snapshot: nil,
		ExportDir: req.ExportDir, SealedEnvEntries: req.SealedEnvEntries,
		EgressAllowlist: req.EgressAllowlist,
	})
}

// Wake brings an instance up, preferring snapshot restore and falling back to
// cold boot. On any terminal error it unwinds every resource it acquired — the
// caller sees no half-built instance and the box leaks nothing (§6.2-4/5).
func (m *Manager) Wake(ctx context.Context, req WakeRequest) (_ *Instance, err error) {
	lease, err := m.alloc.Acquire(req.Instance)
	if err != nil {
		return nil, fmt.Errorf("wake %s: acquire lease: %w", req.Instance, err)
	}
	// Any failure from this point — wire-side allowlist validation
	// included — must fully clean up. Registering the cleanup BEFORE
	// the validation loop is load-bearing: the lease is acquired, so
	// fail-closed early-return paths otherwise leak the slot. The
	// validator (cidr parse + v4/non-/0 checks) sits AFTER the defer
	// on purpose; see ADR-031 + PR #159 review F3.
	defer func() {
		if err != nil {
			m.cleanup(context.WithoutCancel(ctx), lease, netns.NewConfig(
				lease.Instance, lease.Netns, lease.VethHost, lease.VethPeer, lease.HostIP,
			))
		}
	}()
	nc := netns.NewConfig(lease.Instance, lease.Netns, lease.VethHost, lease.VethPeer, lease.HostIP)
	nc.EgressMbit = req.EgressMbit
	// Spec §7 conntrack cap (ADR-018 deferral). Platform-wide constant;
	// not propagated through vmmd gRPC because every instance sees the
	// same value (the failure mode is host-table exhaustion, shared).
	// netns.Config omits the rule when ConntrackCap <= 0 so a vmmd that
	// hasn't been rebuilt still wakes cleanly.
	nc.ConntrackCap = m.conntrackCap
	// ADR-031 + ADR-032 — translate the wire-level CIDR strings into
	// netip.Prefix once, here, so the nft renderer never touches
	// stringly-typed addresses. apid's PATCH handler already
	// ParsePrefix'd these on input and the apps.egress_allowlist
	// cidr[] TRIGGER (`apps_egress_allowlist_cidr`, migration 00033)
	// rejects families outside {4,6} and any /0, so a parse failure
	// at this layer means the wire contract is violated — fail fast
	// rather than silently emit a half-formed ruleset (a single bad
	// CIDR would otherwise crash nft). Defence in depth: wire-side
	// gate here too, so a wire-bypass (e.g. a vmmd that forgets to
	// re-validate) cannot smuggle a bad prefix past apid. ADR-032 —
	// the v4-only gate was removed; v4 + v6 are both allowed here.
	// Bits()==0 mirrors the apid gate so a wire-bypass cannot land a
	// /0 either. On reject the named-return `err` triggers the cleanup
	// defer registered above (release the slot).
	if len(req.EgressAllowlist) > 0 {
		nc.EgressAllowlist = make([]netip.Prefix, 0, len(req.EgressAllowlist))
		for _, c := range req.EgressAllowlist {
			prefix, perr := netip.ParsePrefix(c)
			if perr != nil {
				return nil, fmt.Errorf("wake %s: egress allowlist: invalid CIDR %q: %w", req.Instance, c, perr)
			}
			if prefix.Bits() == 0 {
				return nil, fmt.Errorf("wake %s: egress allowlist: rejected %q (masklen /0; ADR-032 non-/0 contract)", req.Instance, c)
			}
			nc.EgressAllowlist = append(nc.EgressAllowlist, prefix)
		}
	}

	if err = m.setupNetwork(ctx, nc); err != nil {
		return nil, fmt.Errorf("wake %s: network setup: %w", req.Instance, err)
	}

	var method WakeMethod
	method, err = m.bringUp(ctx, lease, nc, req)
	if err != nil {
		return nil, err
	}

	// G2: stage sealed env → unseal each entry → merge into envelope →
	// loopback-mounted write → umount. The Manager is the unseal point
	// (holds host.age). We refuse the request if any sealed blob was
	// supplied without a key configured — silent drop would mean plaintext
	// ciphertext never reaches the guest and the caller's "wake succeeded"
	// hides a missing secret.
	if len(req.SealedEnvEntries) > 0 {
		if m.hostIdentity == nil {
			return nil, fmt.Errorf("wake %s: %w", req.Instance, ErrNoHostKey)
		}
		// We loop-and-merge rather than unseal-into-buf because each entry
		// is a sealed full envelope (per-key rows). That's the natural shape
		// coming from apid's per-row upserts.
		merged := secretbox.Envelope{}
		for _, e := range req.SealedEnvEntries {
			inner, err := secretbox.Open(m.hostIdentity, e.Ciphertext)
			if err != nil {
				return nil, fmt.Errorf("wake %s: open sealed env[%s]: %w",
					req.Instance, logsanitize.Field(e.Key), err)
			}
			for k, v := range inner {
				// Last write wins on key collision. apid upserts on a single
				// row at a time, so collisions can only happen across wake
				// scheduling — meaning a stale row got in; the newer one is
				// the truth.
				merged[k] = v
			}
		}
		// Re-marshal as canonical JSON so guest-init reads the same envelope
		// shape secretbox.Open returns. The plaintext never escapes into any
		// log line — only the size and key count are observable above.
		blob, err := jsonMarshalEnvelope(merged)
		if err != nil {
			return nil, fmt.Errorf("wake %s: marshal envelope: %w", req.Instance, err)
		}
		if err := m.stageSecretsEnv(req.Instance, blob); err != nil {
			return nil, fmt.Errorf("wake %s: stage secrets.env: %w", req.Instance, err)
		}
	}

	// memory.max fence (spec §4.4) — written AFTER bringUp returns because
	// the scope is created by jailer during Boot/Restore and does not exist
	// before then. writeMemoryMax is naturally idempotent (cgroupv2 accepts
	// an identical-value write as a no-op), so snapshot-restore Wake does
	// not need a reset. Failure routes through the deferred cleanup path —
	// the VM is already up, but teardown kills it and releases the lease.
	if err = writeMemoryMax(req.Instance, req.MemSizeMiB); err != nil {
		return nil, fmt.Errorf("wake %s: cgroup fence: %w", req.Instance, err)
	}

	inst := &Instance{Lease: lease, Net: nc, Method: method, AppID: req.AppID}
	// Capture the allowlist rule handles for the in-place patch
	// (PR-B, UpdateEgressAllowlist). The kernel assigns a handle
	// to every `nft add rule`; we re-list the chain with `-a` and
	// match on the iifname + daddr substring to record the
	// handle. Best-effort: a failure to capture (the chain
	// re-list exits non-zero if the netns was torn down
	// concurrently, or the substring doesn't match a renderer
	// invariant) leaves the handle at 0, which means the first
	// UpdateEgressAllowlist for this instance will `add` the new
	// rule alongside the prior one. The next patch will then
	// have the prior handle cached and can `delete` + `add` as
	// intended.
	hV4, hV6, herr := m.captureAllowlistHandles(ctx, nc.Netns)
	if herr != nil {
		m.log.Debug("fcvm: Wake handle capture best-effort failed",
			"instance", req.Instance, "netns", nc.Netns, "err", herr)
	}
	inst.AllowlistHandleV4 = hV4
	inst.AllowlistHandleV6 = hV6
	m.mu.Lock()
	m.live[req.Instance] = inst
	if req.ExportDir != "" {
		m.exportDirs[req.Instance] = req.ExportDir
	}
	m.mu.Unlock()
	m.log.Info("wake ok", "instance", req.Instance, "method", method.String(),
		"uid", lease.UID, "host_ip", lease.HostIP.String())
	return inst, nil
}

// bringUp performs restore-or-cold-boot into an already-networked netns. A
// restore miss or failure is NOT terminal — it falls back to cold boot (ADR-005).
// The returned method is what actually happened: a restore that fell back reads
// WakeColdBoot, so schedd can mark the snapshot stale and schedule a re-snapshot.
// A non-nil error means even cold boot failed (a real wake failure).
func (m *Manager) bringUp(ctx context.Context, lease Lease, nc netns.Config, req WakeRequest) (WakeMethod, error) {
	if PlanWake(req.Snapshot, m.fcVersion) == WakeRestore {
		rs := RestoreSpec{
			VMStatePath: req.Snapshot.VMStatePath,
			// #96 / ADR-025 axis 2: thread the canonical storage key the
			// scheduler populated into WakeRequest.Snapshot. The VMM
			// resolves it through the StorageBackend before staging.
			StorageKey: req.Snapshot.StorageKey,
			// #121 / ADR-025 axis 2 slice 4: thread the canonical
			// vmstate storage key when the engine populated it (remote
			// nodes). Default-local single-box leaves this empty so
			// the VMM falls back to RestoreSpec.VMStatePath above,
			// preserving the legacy host-path branch.
			VMStateStorageKey: req.Snapshot.VMStateStorageKey,
			Tap:               nc.Tap,
			// The restored VM re-reads kernel + drives under the chroot
			// basenames; Park→Kill erased the previous chroot, so hand the
			// Manager.ColdBoot equivalents back to the VMM to re-stage.
			KernelKey: m.paths.Kernel,
			BaseKey:   req.BaseKey,
			LayerKey:  req.LayerKey,
			// ADR-022: same vsock device the cold-boot path attaches, derived
			// from the lease's slot so the guest's listener is reachable at a
			// globally unique guest_cid.
			VsockDevice: NewVsockDevice(lease.Slot),
		}
		if rErr := m.vmm.Restore(ctx, lease, rs); rErr == nil {
			return WakeRestore, nil
		} else {
			// Fall back to cold boot into the same netns; kill any half-restored VM.
			// The wrapped rErr names the failure mode (vsock dial timeout vs
			// ack-nack vs /snapshot/load failure) so the operator doesn't have
			// to dig through vmm.go to find out why the resume hook fired.
			m.log.Warn("restore failed, falling back to cold boot",
				"instance", req.Instance,
				"err", rErr,
				"slot", lease.Slot)
			m.metrics.ObserveFallback()
			_ = m.vmm.Kill(ctx, lease)
		}
	}

	spec := ColdBootSpec{
		KernelKey:  m.paths.Kernel,
		BaseKey:    req.BaseKey,
		LayerKey:   req.LayerKey,
		VcpuCount:  req.VcpuCount,
		MemSizeMiB: req.MemSizeMiB,
		Tap:        nc.Tap,
	}
	if err := m.vmm.BootColdBoot(ctx, lease, spec); err != nil {
		return WakeColdBoot, fmt.Errorf("wake %s: cold boot: %w", req.Instance, err)
	}
	return WakeColdBoot, nil
}

// Park snapshots a running instance then destroys it, freeing all resident RAM
// (invariant §6.2-4: a parked app's cgroup is gone). The snapshot files are
// written to spec's paths. Returns the snapshot info for schedd/imaged to record.
func (m *Manager) Park(ctx context.Context, instance string, spec SnapshotSpec) (SnapshotInfo, error) {
	m.mu.Lock()
	inst, ok := m.live[instance]
	m.mu.Unlock()
	if !ok {
		return SnapshotInfo{}, fmt.Errorf("park %s: not live", instance)
	}

	info, err := m.vmm.Snapshot(ctx, inst.Lease, spec)
	if err != nil {
		// The VM may be in an unknown state; destroy it so nothing leaks. The
		// caller keeps the app cold-bootable (its rootfs is intact).
		_ = m.Destroy(ctx, instance)
		return SnapshotInfo{}, fmt.Errorf("park %s: snapshot: %w", instance, err)
	}
	// Snapshot already destroyed the VM process; release network + lease. cleanup
	// also calls Kill, which is an idempotent no-op on the already-gone VM.
	m.mu.Lock()
	delete(m.live, instance)
	m.mu.Unlock()
	m.cleanup(ctx, inst.Lease, inst.Net)
	m.log.Info("parked", "instance", instance, "mem_bytes", info.MemBytes)
	return info, nil
}

// Destroy stops an instance and releases all its resources. Idempotent: an
// unknown instance is a no-op (already gone). App-VM callers use this; builder
// VMs use DestroyWithExport to surface the build's exit code and copy out
// produced artifacts.
func (m *Manager) Destroy(ctx context.Context, instance string) error {
	_, err := m.DestroyWithExport(ctx, instance, "")
	return err
}

// DestroyWithExport is the builder-VM teardown. It blocks until the
// firecracker child exits, captures the exit code, and copies build artifacts
// into exportDir (loopback-mounted from the chroot). See
// pkg/fcvm/vmm.go::DestroyWithExport for the full contract.
//
// Returns the captured exit code (0 for app VMs / unknown instances). Like
// Destroy, it tears down network + lease on the success path; on failure it
// still runs cleanup (invariant §6.2-4/5).
func (m *Manager) DestroyWithExport(ctx context.Context, instance, exportDir string) (int, error) {
	m.mu.Lock()
	inst, ok := m.live[instance]
	if ok {
		delete(m.live, instance)
	}
	m.mu.Unlock()
	if !ok {
		// Already gone — still safe to export (idempotent), and the exit code
		// is meaningless here.
		if exportDir != "" {
			_ = m.vmm // touch nothing; vmmd's recursion handles unknown
		}
		code, err := m.vmm.DestroyWithExport(ctx, Lease{Instance: instance}, exportDir)
		return code, err
	}
	code, err := m.vmm.DestroyWithExport(ctx, inst.Lease, exportDir)
	// Teardown uses a context detached from the caller's: if the caller's ctx
	// has already expired (test deadline, caller gave up), we still owe the
	// invariant §6.2-4/5 cleanup. Without this, a 30s test deadline firing
	// mid-Destroy leaves the netns + cgroup on disk; observed on the Lima
	// arm64 metal path where nested-KVM cold boot can take >25s. The vmm wait
	// above used the original ctx and is allowed to be cancelled by it.
	m.cleanup(context.WithoutCancel(ctx), inst.Lease, inst.Net)
	m.mu.Lock()
	delete(m.exportDirs, instance)
	m.mu.Unlock()
	if err != nil {
		return code, err
	}
	m.log.Info("destroyed", "instance", instance, "exit_code", code)
	return code, nil
}

// LiveCount reports how many instances the Manager currently tracks.
func (m *Manager) LiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

// LeasedCount reports how many allocator slots are held. After a clean teardown
// of everything, LiveCount and LeasedCount must both be zero — the leak check.
func (m *Manager) LeasedCount() int { return m.alloc.InUse() }

// ExportDirFor returns the host export dir registered for an instance at
// Wake/ColdBoot time (M6 builder VMs only). Returns "" for unknown or app VMs.
// The caller MUST treat the returned path as opaque — it's a host directory
// the goroutine that called Wake chose, and it survives only until the
// instance is removed (DestroyWithExport).
func (m *Manager) ExportDirFor(instance string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exportDirs[instance]
}

// NetnsFor returns the network namespace name (fc-<instance>) the
// Manager bound to this instance at Wake time, plus a boolean that
// reports whether the instance is currently live. Empty string +
// false for unknown instances. The vmmd ForwardHTTP handler
// (pkg/vmmdgrpc/forward.go, issue #98 / ADR-028) uses this to nsenter
// the per-instance netns and dial netns.GuestIP:netns.AppPort on the
// inner side. The boolean is the only race-free liveness signal:
// callers should not try to look the instance up in `m.live`
// directly because Destroy removes the entry.
func (m *Manager) NetnsFor(instance string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.live[instance]
	if !ok {
		return "", false
	}
	return inst.Lease.Netns, true
}

// UpdateEgressAllowlist (ADR-031 + ADR-033, tier-2 PR-B, Track B):
// walks Manager.live, and for each instance whose app_id matches
// the request, applies the new egress allowlist in-place via
// incremental nft patch — no netns teardown, no cold-wake tax
// on the next request. Per-family partition matches the renderer
// (prefix.Addr().Is4() → ip faas forward for v4, ip6 faas forward
// for v6).
//
// The patch strategy:
//
//  1. For each matching live instance, snapshot the cached
//     (Netns, priorEgressAllowlist, priorAllowlistHandleV4,
//     priorAllowlistHandleV6) under m.mu. Released before any
//     netns exec so Wake/Park/Destroy don't see a held Manager
//     lock.
//  2. Per family (v4 then v6):
//     a. If the prior handle is non-empty, emit
//     `nft delete rule ip[6] faas forward handle <H>`.
//     b. Emit `nft add rule ip[6] faas forward … accept` (or
//     skip when the new family half is empty) using the
//     renderer's ForwardAllowlistRule / ForwardAllowlistRule6
//     argv builders.
//  3. On any nft failure mid-patch, revert by re-rendering the
//     prior argv (the cached priorAllowlistHandle* is the
//     invariant) and re-applying it. The revert runs synchronously
//     before returning; a revert failure is returned to the
//     caller as an Internal problem (the live netns is then in
//     an undefined state and schedd's watchdog will Park + ColdBoot
//     the affected instances on its next tick).
//
// Idempotency: identical allowlist re-pushed → samePrefixSet
// fast-path returns nil without running nft. The next cold boot
// re-reads the column, so a snapshot-restore Wake always sees the
// current allowlist — there is no `egressAllowlistVersion` column
// to keep in sync.
//
// Lock order:
//   - m.mu held briefly to snapshot targets and to update the
//     cached handles after a successful patch.
//   - m.mu released before any per-netns nft exec. The kernel
//     serialises nft operations per-netns (the netlink socket is
//     per-call), so concurrent UpdateEgressAllowlist calls on
//     different netns are safe; concurrent calls on the same
//     netns serialise at the nft level.
//
// On an empty allowlist, the per-family add argv is skipped
// (matches the Wake contract for empty EgressAllowlist: no rule
// emitted, chain-policy stays accept). When the prior allowlist
// was non-empty, the prior rule's handle is still deleted so the
// netns returns to the empty-allowlist state.
func (m *Manager) UpdateEgressAllowlist(ctx context.Context, appID string, allowlist []netip.Prefix) error {
	if appID == "" {
		return fmt.Errorf("fcvm: UpdateEgressAllowlist: empty app_id")
	}

	// Snapshot the targets + their cached handles under the
	// manager lock. Released before any netns exec. The full
	// netns.Config is captured so the renderer can produce
	// the same argv as at Wake (Tap name, etc., must match).
	type patchTarget struct {
		instanceID string
		netns      string
		net        netns.Config
		prior      []netip.Prefix
		handleV4   uint64
		handleV6   uint64
	}
	var targets []patchTarget
	m.mu.Lock()
	for id, inst := range m.live {
		if inst.AppID != appID {
			continue
		}
		prior := make([]netip.Prefix, len(inst.Net.EgressAllowlist))
		copy(prior, inst.Net.EgressAllowlist)
		targets = append(targets, patchTarget{
			instanceID: id,
			netns:      inst.Net.Netns,
			net:        inst.Net,
			prior:      prior,
			handleV4:   inst.AllowlistHandleV4,
			handleV6:   inst.AllowlistHandleV6,
		})
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return nil // no live instances for this app — idempotent
	}

	// Compute the new partition's argv via the per-instance
	// netns.Config (the live Tap name threads through so the
	// emitted `iifname "tap0"` matches what the Wake-time
	// rule installed).
	type newAllowlist struct {
		v4Argv []string
		v6Argv []string
	}
	build := func(t patchTarget) newAllowlist {
		nc := t.net
		nc.EgressAllowlist = allowlist
		nx := func(parts ...string) []string {
			return append([]string{"ip", "netns", "exec", t.netns, "nft"}, parts...)
		}
		return newAllowlist{
			v4Argv: nc.ForwardAllowlistRule(func(parts ...string) []string { return append([]string{}, nx(parts...)...) }),
			v6Argv: nc.ForwardAllowlistRule6(func(parts ...string) []string { return append([]string{}, nx(parts...)...) }),
		}
	}

	// Apply per-instance. A failure on any one surfaces to the
	// caller; the loop stops (the caller logs + retries on its
	// next reconcile). Per-instance revert is best-effort: a
	// revert that itself fails is logged at Warn and the error
	// from the original patch is returned (the live netns is
	// then in an undefined state; schedd's watchdog will Park +
	// ColdBoot it on the next tick).
	newHandles := make(map[string]struct{ v4, v6 uint64 }, len(targets))
	for _, t := range targets {
		// Idempotent fast-path: if the prior allowlist is
		// set-equal to the new one, the live netns already
		// matches and the nft exec would be a no-op anyway
		// (delete-then-add the same rule). Skip both the
		// argv build and the nft exec — schedd's pg_notify
		// redelivery lands here on reconnect.
		if samePrefixSet(t.prior, allowlist) {
			newHandles[t.instanceID] = struct{ v4, v6 uint64 }{v4: t.handleV4, v6: t.handleV6}
			continue
		}
		next := build(t)
		newH, err := m.applyOneInstancePatch(ctx, t.netns, t.prior, next.v4Argv, next.v6Argv, t.handleV4, t.handleV6)
		if err != nil {
			return fmt.Errorf("fcvm: UpdateEgressAllowlist app=%s netns=%s: %w", appID, t.netns, err)
		}
		newHandles[t.instanceID] = newH
	}

	// Update cached handles + prior lists so the next patch's
	// fast-path compares against the new baseline.
	m.mu.Lock()
	for id, inst := range m.live {
		nh, ok := newHandles[id]
		if !ok {
			continue
		}
		inst.Net.EgressAllowlist = make([]netip.Prefix, len(allowlist))
		copy(inst.Net.EgressAllowlist, allowlist)
		inst.AllowlistHandleV4 = nh.v4
		inst.AllowlistHandleV6 = nh.v6
	}
	m.mu.Unlock()
	return nil
}

// applyOneInstancePatch runs the per-family patch sequence for a
// single netns. Returns the handles of the freshly-installed v4 /
// v6 allowlist rules (zero when the family half is empty / no
// rule was emitted).
//
// The returned handle values come from the nft argv sequence we
// emit: a fresh `nft add rule ip faas forward …` produces a
// handle that the kernel assigns; we capture it by re-listing the
// chain with `nft -a list chain` and matching on the argv
// substring. That second read is what the unit suite's
// fakeRunner can't simulate (the kernel isn't there), so for
// tests we accept handle == 0 as "unknown, will be re-captured
// on the next patch's pre-read" — the production runner with a
// real `nft` resolves the handle.
//
// Sequence:
//
//  1. Delete prior v4 rule by handle (skip if handle == 0).
//  2. Add new v4 rule (skip if new argv is nil).
//  3. Same for v6.
//  4. On any failure, run the revert: re-add the prior rule by
//     re-rendering its argv. The prior argv was the one cached
//     on the live-instance struct (kept across patches).
//
// Per-family revert: the v4 and v6 patch sequences are run
// independently so that a v4 success isn't rolled back when the
// v6 patch fails. The reverted family re-renders the prior
// allowlist into its argv and re-adds the rule; the successful
// family is left alone. The catch is that handle capture only
// knows the new handle for the family that succeeded (the other
// stays at the prior handle, which is still valid for the
// reverted-on-failure family).
func (m *Manager) applyOneInstancePatch(
	ctx context.Context,
	netnsName string,
	prior []netip.Prefix,
	v4New, v6New []string,
	handleV4, handleV6 uint64,
) (struct{ v4, v6 uint64 }, error) {
	var zero struct{ v4, v6 uint64 }
	// Caller already short-circuited the set-equal case via
	// samePrefixSet before reaching here, so we always run the
	// per-family patch sequence. `prior` is still used below on
	// the failure-path revert (re-render the prior allowlist
	// into the per-family argv and re-add the failed family).
	// Build the per-family patch sequence. Track which family
	// each step belongs to so a mid-sequence failure can revert
	// only the failed family.
	nx := func(parts ...string) []string {
		return append([]string{"ip", "netns", "exec", netnsName, "nft"}, parts...)
	}
	type familyOp struct {
		family  string // "v4" or "v6"
		argv    []string
		newArgv []string // for revert: the new argv that was just added; on revert we re-add it then the next patch will delete by handle
	}
	var ops []familyOp
	if handleV4 > 0 {
		ops = append(ops, familyOp{
			family: "v4",
			argv:   nx("delete", "rule", "ip", "faas", "forward", "handle", fmt.Sprintf("%d", handleV4)),
		})
	}
	if v4New != nil {
		ops = append(ops, familyOp{family: "v4", argv: v4New, newArgv: v4New})
	}
	if handleV6 > 0 {
		ops = append(ops, familyOp{
			family: "v6",
			argv:   nx("delete", "rule", "ip6", "faas", "forward", "handle", fmt.Sprintf("%d", handleV6)),
		})
	}
	if v6New != nil {
		ops = append(ops, familyOp{family: "v6", argv: v6New, newArgv: v6New})
	}
	// Idempotent fast-path: nothing to do.
	if len(ops) == 0 {
		return zero, nil
	}

	// Run per-family patches. A failure on one family reverts
	// that family (re-adds the prior rule) and returns the
	// error — the other family is left at its new state.
	failedFamily := ""
	patchErr := error(nil)
	for _, op := range ops {
		if err := m.runCommands(ctx, [][]string{op.argv}); err != nil {
			failedFamily = op.family
			patchErr = err
			break
		}
	}
	if failedFamily != "" {
		// Per-family revert: re-render the prior allowlist for
		// the failed family and re-add it. The other family is
		// untouched (its new ruleset is already live).
		priorNC := netns.Config{Netns: netnsName, EgressAllowlist: prior}
		var revertArgv []string
		if failedFamily == "v4" {
			revertArgv = priorNC.ForwardAllowlistRule(func(parts ...string) []string { return append([]string{}, nx(parts...)...) })
		} else {
			revertArgv = priorNC.ForwardAllowlistRule6(func(parts ...string) []string { return append([]string{}, nx(parts...)...) })
		}
		if revertArgv != nil {
			if rerr := m.runCommands(ctx, [][]string{revertArgv}); rerr != nil {
				m.log.Warn("fcvm: UpdateEgressAllowlist revert failed; live netns may be in undefined state",
					"netns", netnsName, "family", failedFamily,
					"patch_err", patchErr, "revert_err", rerr)
			}
		}
		return zero, patchErr
	}

	// Handle capture: the kernel assigns handles on add. We
	// re-list the chain (when captureRunner is wired) and parse
	// the new handle so the next patch's delete-by-handle call
	// targets the rule that was just installed. When the
	// capture runner is nil (unit tests with a fakeRunner that
	// doesn't simulate the kernel), the cached handle stays at
	// the prior value — the metal test exercises the
	// captureRunner path on the EX44.
	newH4, newH6 := handleV4, handleV6
	if m.captureRunner != nil {
		if h, err := listChainHandles(ctx, m.captureRunner, netnsName, "ip", "faas", "forward"); err == nil {
			newH4 = h
		}
		if h, err := listChainHandles(ctx, m.captureRunner, netnsName, "ip6", "faas", "forward"); err == nil {
			newH6 = h
		}
	}
	return struct{ v4, v6 uint64 }{v4: newH4, v6: newH6}, nil
}

// samePrefixSet compares two prefix slices for set equality.
// Order independent (the renderer's partition is by family, not
// by input order). Used by UpdateEgressAllowlist's idempotent
// fast-path.
func samePrefixSet(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
next:
	for _, pa := range a {
		for i, pb := range b {
			if used[i] {
				continue
			}
			if pa == pb {
				used[i] = true
				continue next
			}
		}
		return false
	}
	return true
}

// setupNetwork realises the per-instance topology (veth/tap/addressing), applies
// the per-plan tc egress cap on the host-side veth, and then loads the
// nftables ruleset that publishes the guest and enforces the egress policy
// (§7/§11). Commands run in order, stopping at the first error; a failure
// leaves the caller's deferred cleanup to unwind everything (invariant §6.2-5).
// The DNAT rules must land before readiness is probed, so they run here, inside
// the setup phase, rather than after bringUp.
//
// Ordering matters on snapshot-restore Wake (the netns outlives the VM):
// each ruleset's reset (`tc qdisc del`, `nft delete table`) runs best-effort
// BEFORE its strict add, so the second `add` does not collide. Both resets
// exit non-zero on a fresh netns / brand-new veth; those failures are
// expected and logged at Debug.
func (m *Manager) setupNetwork(ctx context.Context, nc netns.Config) error {
	if err := m.runCommands(ctx, nc.SetupCommands()); err != nil {
		return err
	}

	// tc egress cap. Best-effort reset (errors expected on fresh veth);
	// strict add runs only when the plan carries a cap. EgressMbit == 0
	// keeps legacy callers (existing fakeRunner tests, debug paths)
	// working without forcing every caller to set a non-zero rate.
	for _, argv := range nc.TcResetCommands() {
		if err := m.run.Run(ctx, argv); err != nil {
			m.log.Debug("tc reset (best-effort, expected on fresh veth)",
				"instance", nc.Instance, "argv", argv, "err", err)
		}
	}
	if nc.EgressMbit > 0 {
		if err := m.runCommands(ctx, nc.TcCommands()); err != nil {
			return fmt.Errorf("tc egress cap: %w", err)
		}
	}

	// nft ruleset reset + strict add. See NftCommands / NftResetCommands
	// doc comments for the established/related ordering that makes
	// published replies survive the lateral-movement deny.
	for _, argv := range nc.NftResetCommands() {
		if err := m.run.Run(ctx, argv); err != nil {
			m.log.Debug("nft reset (best-effort, expected on fresh netns)",
				"instance", nc.Instance, "argv", argv, "err", err)
		}
	}
	return m.runCommands(ctx, nc.NftCommands())
}

// runCommands runs each argv in order, stopping at the first error. The argv
// is included in the wrapped error so the failure is identifiable in logs.
func (m *Manager) runCommands(ctx context.Context, cmds [][]string) error {
	for _, argv := range cmds {
		if err := m.run.Run(ctx, argv); err != nil {
			return fmt.Errorf("%v: %w", argv, err)
		}
	}
	return nil
}

// CaptureRunner is the stdout-aware slice of the host command
// runner. The plain Runner interface only returns error; the
// in-place allowlist patch (PR-B) needs to read `nft -a list
// chain` output to resolve the kernel-assigned handle of the
// just-added rule. Production wires an adapter that wraps
// exec.CommandContext with CombinedOutput; unit tests stub
// through WithCaptureRunner. Nil is a valid value: the wake path
// then leaves AllowlistHandle{V4,V6} at 0 (the orphan rule
// stays correct, the next patch picks it up via listChainHandles).
type CaptureRunner interface {
	RunCapture(ctx context.Context, argv []string) ([]byte, error)
}

// WithCaptureRunner installs the capture runner post-construction.
// Returns the receiver so cmd/vmmd can chain it on NewManager.
//
//	vmm := fcvm.NewManager(...).WithCaptureRunner(cap)
func (m *Manager) WithCaptureRunner(cap CaptureRunner) *Manager {
	m.captureRunner = cap
	return m
}

// captureAllowlistHandles (tier-2 PR-B, called from Wake after
// setupNetwork + runCommands(NftCommands)) reads the kernel-
// assigned nft handle of each per-family allowlist accept rule
// just emitted by the renderer. Best-effort: returns (0, 0, nil)
// when (a) the capture runner is nil, (b) the chain is empty
// (no rule was emitted — empty EgressAllowlist), or (c) the
// handle substring can't be matched against the renderer
// invariant. The metal test exercises the success path; the
// unit suite stubs the runner to verify the Wake path tolerates
// capture failure.
func (m *Manager) captureAllowlistHandles(ctx context.Context, netnsName string) (uint64, uint64, error) {
	if m.captureRunner == nil {
		return 0, 0, nil
	}
	hV4, errV4 := listChainHandles(ctx, m.captureRunner, netnsName, "ip", "faas", "forward")
	if errV4 != nil {
		return 0, 0, errV4
	}
	hV6, errV6 := listChainHandles(ctx, m.captureRunner, netnsName, "ip6", "faas", "forward")
	if errV6 != nil {
		return 0, 0, errV6
	}
	return hV4, hV6, nil
}

// listChainHandles runs `ip netns exec <ns> nft -a list chain
// <family> faas forward` and returns the handle of the rule that
// matches the allowlist-renderer invariant
// `iifname "<tap>" <family> daddr { … } accept`. Returns 0 when
// no such rule is present (empty allowlist on that family half).
//
// The renderer always emits tap0 (identical in every netns per
// ADR-009) so the substring is well-defined. If a future renderer
// lets tap vary per instance, this helper needs to take the tap
// name as a parameter.
func listChainHandles(ctx context.Context, cap CaptureRunner, netnsName, family, table, chain string) (uint64, error) {
	argv := []string{"ip", "netns", "exec", netnsName, "nft", "-a", "list", "chain", family, table, chain}
	out, err := cap.RunCapture(ctx, argv)
	if err != nil {
		return 0, fmt.Errorf("nft -a list chain %s %s %s: %w", family, table, chain, err)
	}
	// Match on the `iifname "tap0"` substring to anchor to the
	// allowlist accept rule (the lateral-movement deny lines don't
	// match). Modern nft prints handles at end-of-rule with
	// `handle N`; the regex below extracts the integer.
	//
	// Output sample:
	//   chain forward {
	//    type filter hook forward priority 0; policy accept;
	//    iifname "tap0" ip daddr { 1.2.3.0/24 } accept # handle 42
	//   }
	needleAllow := `iifname "tap0" ` + family + ` daddr`
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, needleAllow) {
			continue
		}
		idx := strings.LastIndex(line, "# handle ")
		if idx < 0 {
			continue
		}
		tail := strings.TrimSpace(line[idx+len("# handle "):])
		// Strip trailing `}` if the chain closes on the same
		// physical line (some nft versions fold the closing brace).
		if i := strings.IndexAny(tail, " }"); i >= 0 {
			tail = tail[:i]
		}
		h, perr := strconv.ParseUint(tail, 10, 64)
		if perr != nil {
			continue
		}
		return h, nil
	}
	return 0, nil
}

// cleanup is the unwind path: best-effort kill the VM, best-effort tear down the
// network, and always release the lease. Errors are logged, never returned — a
// cleanup that gives up would leak.
func (m *Manager) cleanup(ctx context.Context, lease Lease, nc netns.Config) {
	if err := m.vmm.Kill(ctx, lease); err != nil {
		m.log.Warn("cleanup: kill vm", "instance", lease.Instance, "err", err)
	}
	for _, argv := range nc.TeardownCommands() {
		if err := m.run.Run(ctx, argv); err != nil {
			// Teardown commands are expected to fail if the resource was never
			// created (e.g. netns del on a boot that failed before netns add).
			m.log.Debug("cleanup: teardown cmd", "cmd", argv, "err", err)
		}
	}
	// cleanup runs exactly once per lease (failed boot OR Destroy, never both),
	// so Release should succeed; a failure here is a real leak signal, not noise.
	if err := m.alloc.Release(lease.Instance); err != nil {
		m.log.Warn("cleanup: release lease", "instance", lease.Instance, "err", err)
	}
}

// discard is an io.Writer sink for the nil-logger fallback.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
