// Proto ↔ fcvm adapters. Kept separate from server.go so each handler stays
// under the §Conventions 50-line limit and so every conversion is in one
// place if a future proto revision lands.

package vmmdgrpc

import (
	"net/netip"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/fcvm"
	"github.com/onebox-faas/faas/pkg/wire"
	"google.golang.org/grpc/codes"
)

// toWakeRequest flattens CreateFromSnapshotRequest into an fcvm.WakeRequest.
// The caller resolves (app) here; vmmd stores none of it (ADR-014).
func toWakeRequest(req *vmmdpb.CreateFromSnapshotRequest) (fcvm.WakeRequest, error) {
	if req.GetInstance() == "" {
		return fcvm.WakeRequest{}, api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing instance", "instance is required").
			WithDocs("https://" + wire.DocsHost + "/vmmd#create")
	}
	app := req.GetApp()
	if app == nil {
		return fcvm.WakeRequest{}, api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing app", "AppSpec is required").
			WithDocs("https://" + wire.DocsHost + "/vmmd#appspec")
	}
	snap := req.GetSnapshot()
	wr := fcvm.WakeRequest{
		Instance:         req.GetInstance(),
		BaseKey:          app.GetBaseKey(),
		LayerKey:         app.GetLayerKey(),
		VcpuCount:        int(app.GetVcpuCount()),
		MemSizeMiB:       int(app.GetMemSizeMib()),
		EgressMbit:       int(app.GetEgressMbit()),
		SealedEnvEntries: sealedFromProto(app.GetSealedEnv()),
		// Issue #395 / ADR-045: plaintext api_env channel. apid
		// enforces the per-plan EnvValueMaxBytes + EnvVarsMax
		// quota upstream; vmmd just forwards to StageAPIEnv which
		// writes /etc/faas/env.json on drive1. Empty slice = no
		// env.json file written (manifest env still flows in via
		// /etc/faas/app.json, the legacy path).
		APIEnvEntries: apiEnvFromProto(app.GetApiEnv()),
		// ADR-031: forward the per-app outbound IP allowlist on the
		// wake wire. apid parses + plan-gates + size-caps upstream;
		// vmmd translates CIDRs into netns.Config.EgressAllowlist on
		// Wake. Empty slice = no allowlist rule (current behaviour).
		EgressAllowlist: app.GetEgressAllowlist(),
		// tier-2 PR-B: schedd fans UpdateEgressAllowlist out by
		// app_id, so the live Instance needs to remember which app
		// it was woken for. The scheduler already knows the app
		// when it calls CreateFromSnapshot; passing it on the wire
		// means vmmd doesn't have to round-trip back to apid.
		AppID: app.GetAppId(),
		// issue #301 / ADR-044 — Plan + AccountID thread the
		// apps-row context onto the wire so vmmd can land the VM
		// under the per-plan cgroup sub-slice
		// (faas-tenant.slice/<plan-slice>/<instance>) and label
		// the vmmd_cpu_throttle_seconds_total counter. Empty
		// Plan falls back to the legacy 2-level path
		// (ParentCgroupRoot/<instance>) for pre-#301 callers;
		// new callers must always populate this.
		Plan:      api.Plan(req.GetPlan()),
		AccountID: req.GetAccountId(),
	}
	if snap != nil {
		// #96 / ADR-025 axis 2 (slice 3) — mem_path is gone from the
		// proto. The StorageBackend is the only carrier for the mem
		// blob; if a caller hands us a SnapshotRef with an empty
		// StorageKey, fall back to cold-boot (the createcoldboot
		// branch) by leaving wr.Snapshot = nil. The Manager treats
		// nil Snapshot as cold-boot, which is exactly the
		// cold-boot-must-always-work guarantee (ADR-005).
		//
		// #121 / ADR-025 axis 2 slice 4 — vmstate_storage_key is the
		// canonical key the vmstate blob lives under when the new
		// StorageBackend carrier is used; vmstate_path is the legacy
		// host-path fallback (default-local single-box). Both flow
		// through unchanged so fcvm.Snapshot.Usable() can accept
		// either locator and pick the right resume path.
		if snap.GetStorageKey() == "" {
			return wr, nil
		}
		wr.Snapshot = &fcvm.Snapshot{
			VMStatePath:       snap.GetVmstatePath(),
			FCVersion:         snap.GetFcVersion(),
			StorageKey:        snap.GetStorageKey(),
			VMStateStorageKey: snap.GetVmstateStorageKey(),
		}
	}
	return wr, nil
}

// toColdBootRequest flattens CreateColdBootRequest into an fcvm.WakeRequest
// with no snapshot. Same validations as toWakeRequest minus snapshot.
func toColdBootRequest(req *vmmdpb.CreateColdBootRequest) (fcvm.WakeRequest, error) {
	if req.GetInstance() == "" {
		return fcvm.WakeRequest{}, api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing instance", "instance is required").
			WithDocs("https://" + wire.DocsHost + "/vmmd#create")
	}
	app := req.GetApp()
	if app == nil {
		return fcvm.WakeRequest{}, api.NewProblem(int(codes.InvalidArgument),
			api.CodeValidation, "Missing app", "AppSpec is required").
			WithDocs("https://" + wire.DocsHost + "/vmmd#appspec")
	}
	return fcvm.WakeRequest{
		Instance:         req.GetInstance(),
		BaseKey:          app.GetBaseKey(),
		LayerKey:         app.GetLayerKey(),
		VcpuCount:        int(app.GetVcpuCount()),
		MemSizeMiB:       int(app.GetMemSizeMib()),
		EgressMbit:       int(app.GetEgressMbit()),
		SealedEnvEntries: sealedFromProto(app.GetSealedEnv()),
		// Issue #395 / ADR-045: see toWakeRequest's APIEnvEntries
		// comment. Cold-boot mirrors the wake path so deploy's
		// first boot primes the same plaintext env layer.
		APIEnvEntries: apiEnvFromProto(app.GetApiEnv()),
		// ADR-031: see toWakeRequest for the rationale; cold-boot
		// mirrors it so deploy primes the same egress policy.
		EgressAllowlist: app.GetEgressAllowlist(),
		// tier-2 PR-B: see toWakeRequest. The cold-boot path is
		// the first boot of a deploy; setting AppID here means
		// the very first UpdateEgressAllowlist fan-out finds the
		// instance via m.live[].AppID without a separate
		// bootstrap path.
		AppID: app.GetAppId(),
		// issue #301 / ADR-044 — see toWakeRequest. Cold-boot
		// mirrors Plan + AccountID so deploy's first boot on a
		// fresh VM lands under the per-plan cgroup sub-slice
		// and the throttle counter labels are populated.
		Plan:      api.Plan(req.GetPlan()),
		AccountID: req.GetAccountId(),
	}, nil
}

// sealedFromProto converts a slice of vmmdpb.SealedSecret into the fcvm
// shape Manager.Wake consumes. Nil in -> nil out (the Manager treats
// nil and empty equivalently: no StageSecretsEnv call). We don't reject
// malformed rows here — the recipient + key validation already happened
// at apid's PUT, and the Manager will surface an Open failure on a
// truly bogus ciphertext.
func sealedFromProto(pbs []*vmmdpb.SealedSecret) []fcvm.SealedEnvEntry {
	if len(pbs) == 0 {
		return nil
	}
	out := make([]fcvm.SealedEnvEntry, 0, len(pbs))
	for _, p := range pbs {
		out = append(out, fcvm.SealedEnvEntry{
			Key:        p.GetKey(),
			Ciphertext: p.GetCiphertext(),
		})
	}
	return out
}

// apiEnvFromProto is the plaintext sibling of sealedFromProto (issue
// #395 / ADR-045). Mirrors the nil-in/nil-out shape — Manager.Wake
// treats nil and empty equivalently: no StageAPIEnv call. We don't
// re-validate key regex or byte cap here; apid's PUT handler enforces
// both against Limits.EnvVarsMax / Limits.EnvValueMaxBytes BEFORE the
// row reaches PG, so by the time the value arrives on the wire it's
// already trusted.
func apiEnvFromProto(pbs []*vmmdpb.APIEnvEntry) []fcvm.APIEnvEntry {
	if len(pbs) == 0 {
		return nil
	}
	out := make([]fcvm.APIEnvEntry, 0, len(pbs))
	for _, p := range pbs {
		out = append(out, fcvm.APIEnvEntry{
			Key:   p.GetKey(),
			Value: p.GetValue(),
		})
	}
	return out
}

// wakeResponseFromInstance builds a WakeResponse from a just-woken instance.
// requestMethod is what the *caller* asked for (WAKE_RESTORE or
// WAKE_COLD_BOOT); the actual method reflects what Manager did (a restore
// that fell back reads WAKE_COLD_BOOT).
func wakeResponseFromInstance(instance string, req fcvm.WakeRequest, inst *fcvm.Instance, requestMethod vmmdpb.WakeMethod) *vmmdpb.WakeResponse {
	return &vmmdpb.WakeResponse{
		Instance:        instance,
		LeaseUid:        int32(inst.Lease.UID),
		HostIp:          addrOrEmpty(inst.Lease.HostIP),
		Netns:           inst.Net.Netns,
		VethHost:        inst.Net.VethHost,
		VethPeer:        inst.Net.VethPeer,
		Method:          wakeMethodFrom(inst.Method),
		RequestedMethod: requestMethod,
	}
}

func wakeMethodFrom(m fcvm.WakeMethod) vmmdpb.WakeMethod {
	if m == fcvm.WakeRestore {
		return vmmdpb.WakeMethod_WAKE_RESTORE
	}
	return vmmdpb.WakeMethod_WAKE_COLD_BOOT
}

// addrOrEmpty renders an addr as a string if valid; "" otherwise. Mirrors
// the netip.Addr.IsValid() guard so callers that hand us Lease.Zero /
// unset addr fields don't produce impossible literal strings.
func addrOrEmpty(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// toEgressAllowlist parses the wire's repeated string of CIDR
// literals into the netip.Prefix slice Manager.UpdateEgressAllowlist
// consumes. The wire carries the same shape as
// AppSpec.egress_allowlist (field 7), so the renderer partition
// (prefix.Addr().Is4()) works unchanged. A malformed entry is
// rejected with a typed Problem: the DB trigger and the apid
// validator already enforce v4-or-v6 + non-/0 upstream — a bad
// entry here is a contract violation that the manager surfaces
// with InvalidArgument rather than silently dropping the rule.
func toEgressAllowlist(ss []string) ([]netip.Prefix, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	out := make([]netip.Prefix, 0, len(ss))
	for _, s := range ss {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, api.NewProblem(int(codes.InvalidArgument),
				api.CodeValidation,
				"Invalid egress_allowlist entry",
				err.Error(),
			)
		}
		out = append(out, p)
	}
	return out, nil
}
