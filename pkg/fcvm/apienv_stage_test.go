// Tests for the api-env staging path (issue #395 / ADR-045):
// cold-wake + restore both merge the per-app plaintext entries into a
// single JSON map, marshal it back to canonical JSON, and pass it to
// the VMM's StageAPIEnv method. Failure modes covered mirror the
// secrets_stage_test.go surface MINUS the unseal + tamper + host-key
// cases (plaintext by contract — no host key involved):
//
//   - empty APIEnvEntries ⇒ no StageAPIEnv call (no-op)
//   - non-empty entries ⇒ StageAPIEnv receives a JSON map, NOT the
//     raw per-row strings (the merge happens at the Manager, not the
//     VMM, so a future refactor that drops the merge is caught here)
//   - stageAPIEnvErr ⇒ the deferred cleanup path runs (no live
//     instance is registered)
//   - multiple per-key entries ⇒ merged into one map, last write wins
//   - secrets + api-env both populated ⇒ both staging methods fire
//     in order; env.json write does not displace secrets.env write
package fcvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/secretbox"
)

func TestWake_EmptyAPIEnv_NoStageCall(t *testing.T) {
	// Manager.Wake with no APIEnvEntries should proceed through
	// bringUp without ever invoking StageAPIEnv. The VMM stub records
	// any stage calls so we can assert none happened. Mirrors
	// TestWake_EmptySealedEnv_NoStageCall.
	vmm := &fakeVMM{}
	m := newTestManager(&fakeRunner{}, vmm)

	cb := req("no-apienv")
	if _, err := m.ColdBoot(context.Background(), cb); err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	if got := len(vmm.stagedAPIEnv); got != 0 {
		t.Errorf("StageAPIEnv called %d times, want 0", got)
	}
}

func TestWake_APIEnv_StageRoundTrip_MergedToJSONMap(t *testing.T) {
	// StageAPIEnv MUST receive the merged JSON map shape, not the
	// raw per-row bytes. The merge happens at the Manager — a
	// future refactor that drops the merge and passes per-row
	// payloads through would break guest-init's reader (which
	// expects a single JSON map).
	vmm := &fakeVMM{}
	m := newTestManager(&fakeRunner{}, vmm)

	cb := req("apienv-rt")
	cb.APIEnvEntries = []APIEnvEntry{
		{Key: "LOG_LEVEL", Value: "debug"},
		{Key: "FEATURE_X", Value: "on"},
	}
	if _, err := m.ColdBoot(context.Background(), cb); err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	if got := len(vmm.stagedAPIEnv); got != 1 {
		t.Fatalf("StageAPIEnv called %d times, want 1", got)
	}
	got := vmm.stagedAPIEnv[0].blob

	var env map[string]string
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("stage blob not valid JSON map: %v (blob=%q)", err, got)
	}
	if env["LOG_LEVEL"] != "debug" || env["FEATURE_X"] != "on" {
		t.Errorf("env.json shape wrong: %+v", env)
	}
	// And the values must NOT appear in the "raw" form (e.g. one
	// big concatenated blob); the JSON object shape is the contract.
	if !strings.HasPrefix(string(got), "{") {
		t.Errorf("stage blob is not a JSON object: %q", got)
	}
}

func TestWake_APIEnv_MultipleEntries_MergedIntoMap(t *testing.T) {
	// Mirrors the secrets merge-semantics test: per-row entries
	// accumulate, last write wins on key collision. The plaintext
	// surface means there's no unseal step — the merge happens at
	// the Manager.Wake call site.
	vmm := &fakeVMM{}
	m := newTestManager(&fakeRunner{}, vmm)

	cb := req("apienv-merge")
	cb.APIEnvEntries = []APIEnvEntry{
		{Key: "A", Value: "alpha"},
		{Key: "B", Value: "beta"},
		{Key: "A", Value: "alpha2"}, // later row wins
	}
	if _, err := m.ColdBoot(context.Background(), cb); err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	got := vmm.stagedAPIEnv[0].blob
	var env map[string]string
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("decode: %v (blob=%s)", err, got)
	}
	if env["A"] != "alpha2" {
		t.Errorf("A=%q want alpha2 (last-write-wins)", env["A"])
	}
	if env["B"] != "beta" {
		t.Errorf("B=%q want beta", env["B"])
	}
}

func TestWake_APIEnv_StageErr_FailsWakeAndCleansUp(t *testing.T) {
	// When StageAPIEnv itself returns an error (e.g. missing drive1
	// chroot), the deferred cleanup runs and the instance does NOT
	// appear in m.live. Mirrors the secrets stage-fail test.
	vmm := &fakeVMM{stageAPIEnvErr: errors.New("drive1 missing")}
	m := newTestManager(&fakeRunner{}, vmm)

	cb := req("apienv-stage-fail")
	cb.APIEnvEntries = []APIEnvEntry{{Key: "K", Value: "v"}}

	if _, err := m.ColdBoot(context.Background(), cb); err == nil {
		t.Fatal("ColdBoot should fail when StageAPIEnv fails")
	}
	if _, ok := m.live["apienv-stage-fail"]; ok {
		t.Errorf("half-staged instance is registered — cleanup did not run")
	}
}

func TestWake_SealedAndAPIEnv_BothStage(t *testing.T) {
	// When an app has both sealed secrets AND api env, both staging
	// paths must fire. The order matters for the guest-init reader
	// (secrets.env is read first, then env.json, then app.json);
	// reordering would silently change the precedence contract.
	id := newIdentity(t)
	blob := sealEnv(t, id, secretbox.Envelope{"STRIPE_KEY": "sk_test"})

	vmm := &fakeVMM{}
	m := newTestManager(&fakeRunner{}, vmm)
	m.SetHostIdentity(id)

	cb := req("both")
	cb.SealedEnvEntries = entriesFromBlob(blob)
	cb.APIEnvEntries = []APIEnvEntry{{Key: "LOG_LEVEL", Value: "debug"}}

	if _, err := m.ColdBoot(context.Background(), cb); err != nil {
		t.Fatalf("ColdBoot: %v", err)
	}
	if len(vmm.stagedSecrets) != 1 {
		t.Errorf("StageSecretsEnv called %d times, want 1", len(vmm.stagedSecrets))
	}
	if len(vmm.stagedAPIEnv) != 1 {
		t.Errorf("StageAPIEnv called %d times, want 1", len(vmm.stagedAPIEnv))
	}
	// Order: secrets staged first (sealed → unmounted chroot),
	// then api-env. The fake captures a monotonic stageCallSeq on
	// every stage call — we assert secrets.seq < apiEnv.seq so a
	// refactor that swaps the call order inside Manager.Wake trips
	// this test loud rather than the precedence contract failing
	// silent. The reason ordering matters: secrets.env is read
	// first inside guest-init, then env.json is layered on top via
	// BuildEnvWithSecrets — reordering would silently change the
	// precedence (secrets > api-env) into a race where the last
	// write wins, breaking the documented contract from ADR-045
	// §Decision "4-layer precedence: OS < Manifest < api-env < Secrets".
	if vmm.stagedSecrets[0].seq >= vmm.stagedAPIEnv[0].seq {
		t.Errorf("stage call order: secrets.seq=%d apiEnv.seq=%d, want secrets < api-env",
			vmm.stagedSecrets[0].seq, vmm.stagedAPIEnv[0].seq)
	}
}
