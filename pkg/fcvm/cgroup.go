package fcvm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/onebox-faas/faas/pkg/api"
)

// cgroupRoot is the canonical cgroup v2 unified mount (spec §3 ADR-008:
// cgroups v1 must be off). Package-level (not const) so cgroup_test.go
// can substitute t.TempDir() under a t.Cleanup. Production callers
// never touch this — they read /sys/fs/cgroup directly.
var cgroupRoot = "/sys/fs/cgroup"

// writePlanCgroup sets memory.max and cpu.max on the per-VM cgroup
// scope jailer creates during Boot/Restore (--parent-cgroup
// `faas-tenant.slice/<plan-slice>` with `jailer --cgroup cpu.weight=N`).
//
// Spec §4.4 line 137: "cgroup v2 scope faas-tenant.slice/{instance}
// with memory.max = plan_mb + 8 MB". Issue #301 / ADR-044 extends
// the hierarchy to 3 levels (`faas-tenant.slice/<plan-slice>/<instance>`)
// so the kernel can enforce per-plan cpu.weight + cpu.max quotas;
// the scope name still equals the Lease.Instance verbatim — see
// PerInstanceScope for the lockstep definition.
//
// The +8 MB is the per-VM overhead accounted for by
// api.PerVMOverheadMB (pkg/api/limits.go).
//
// Note: the original spec text used `vm-{instance}.scope`; jailer
// v1.7's --id validator rejects '.' (panic: "Invalid char (.) at
// position N"), so we use the bare instance name and rely on the
// filter in pkg/fcvm/leakcheck/residentbytes.go to exclude
// systemd-installed siblings (init.scope, user.slice, etc.).
//
// The scope MUST already exist by the time this runs: Manager.Wake
// calls writePlanCgroup only after bringUp returns successfully, and
// bringUp blocks on firecracker readiness (which means jailer has
// already joined the scope). If the scope is absent, the IsNotExist
// branch produces a clear diagnostic that names the missing scope —
// distinct from a generic permission failure, so on-metal diagnosis
// doesn't waste time guessing.
//
// Both writes are naturally idempotent: cgroupv2 accepts a new
// memory.max / cpu.max with the same value as a no-op. Snapshot-restore
// Wake can call this on every wake without a separate reset (unlike tc
// qdisc, which collides).
//
// cpu.max is a direct file write (not a jailer --cgroup arg) because
// jailer v1.7 only exposes cpu.weight and memory.max through --cgroup;
// the quota must land in cpu.max so the kernel enforces it.
func writePlanCgroup(instance string, plan api.Plan, planMB int) error {
	if planMB < 1 {
		return fmt.Errorf("fcvm: cgroup: planMB %d < 1", planMB)
	}
	if !plan.Valid() {
		return fmt.Errorf("fcvm: cgroup: invalid plan %q (issue #301 / ADR-044)", plan)
	}
	scope := filepath.Join(cgroupRoot, ParentCgroupFor(plan), PerInstanceScope(instance))
	if err := writeMemoryMaxTo(scope, planMB); err != nil {
		return err
	}
	if err := writeCPUMaxTo(scope, plan); err != nil {
		return err
	}
	return nil
}

// writeMemoryMaxTo writes memory.max (in bytes) into the given
// fully-resolved scope path. Idempotent. Public so the cgroup unit
// test can exercise it directly without spinning up a Manager.
func writeMemoryMaxTo(scope string, planMB int) error {
	bytes := int64(api.BillableRAMMB(planMB)) << 20
	path := filepath.Join(scope, "memory.max")
	body := fmt.Sprintf("%d\n", bytes)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("fcvm: cgroup: scope %s missing (jailer did not create it): %w", scope, err)
		}
		return fmt.Errorf("fcvm: cgroup: write %s: %w", path, err)
	}
	return nil
}

// writeCPUMaxTo writes cpu.max (in microseconds) into the given
// fully-resolved scope path. Idempotent. Same Newline-terminated
// format as systemd-run — matches the kernel parser's expectation.
//
// cpu.max format is "<quota> <period>" microseconds. An empty plan
// row would write "0 100000" which the kernel treats as "no quota"
// (an unconstrained slice); we validate the plan above so this
// branch is unreachable in production.
func writeCPUMaxTo(scope string, plan api.Plan) error {
	quota := plan.CPUQuotaUS()
	period := plan.CPUPeriodUS()
	if quota <= 0 || period <= 0 {
		// Fail closed: a missing quota is a missing-row plan,
		// which Manager.Wake rejected upstream. The check here
		// is defence-in-depth so a future caller that bypasses
		// Manager.Wake doesn't silently emit an unbounded slice.
		return fmt.Errorf("fcvm: cgroup: plan %q has non-positive cpu.max (%d/%d)", plan, quota, period)
	}
	path := filepath.Join(scope, "cpu.max")
	body := fmt.Sprintf("%d %d\n", quota, period)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("fcvm: cgroup: scope %s missing (jailer did not create it): %w", scope, err)
		}
		return fmt.Errorf("fcvm: cgroup: write %s: %w", path, err)
	}
	return nil
}

// writeMemoryMax is the legacy single-field writer kept so the
// cgroup_test.go unit tests can call the old signature without
// routing through a plan. It writes ONLY memory.max — cpu.max is
// the per-plan enforcement (issue #301 / ADR-044) and the legacy
// callers never had a plan to pass. New callers must use
// writePlanCgroup for the production path; this legacy shim
// exists for the unit-test surface only. The scope path uses the
// legacy 2-level parent (ParentCgroupRoot directly, no plan slice)
// to match what the pre-issue-301 unit tests assert.
func writeMemoryMax(instance string, planMB int) error {
	if planMB < 1 {
		return fmt.Errorf("fcvm: cgroup: planMB %d < 1", planMB)
	}
	scope := filepath.Join(cgroupRoot, ParentCgroupRoot, PerInstanceScope(instance))
	return writeMemoryMaxTo(scope, planMB)
}
