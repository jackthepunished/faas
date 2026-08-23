package main

// commands_decompose_warn_test.go — unit tests for the ADR-124
// ship-blocker #4 PrintWarn hook in printPlanText. The warn fires
// when --exclude produced a destructive subset (plan.Removed
// non-empty) AND the operator did not ask for the full partition
// view (showAffected=false). All other combinations must stay
// silent so the default render stays terse.
//
// Pins:
//
//  1. (exclude=non-empty, Removed=non-empty, showAffected=false)
//     → PrintWarn fires (the destructive-nudge case)
//  2. (exclude=non-empty, Removed=non-empty, showAffected=true)
//     → no warn (operator already sees the partition)
//  3. (exclude=empty,    Removed=non-empty, showAffected=false)
//     → no warn (no --exclude → no destructive intent)
//  4. (exclude=non-empty, Removed=empty,    showAffected=false)
//     → no warn (--exclude did not produce soft-deletes)
//  5. (exclude=non-empty, Removed=non-empty, showAffected=false)
//     → ! WARNING glyph prefix (writeStatus contract)

import (
	"bytes"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/api"
)

// basePlan returns a minimal PlanResponse with the fields
// printPlanText reads. can_apply=true keeps the test focused on
// the warn path (the can_apply=false branch returns early).
func basePlan() api.PlanResponse {
	return api.PlanResponse{
		ProjectSlug:   "demo",
		ScanSource:    "compose",
		Tier:          "single",
		ObservedApps:  1,
		LimitApps:     5,
		ObservedCrons: 0,
		LimitCrons:    10,
		CanApply:      true,
		Removed:       []string{"checkout-api"},
	}
}

// TestPrintPlanText_WarnsOnDestructiveExclude pins the load-
// bearing case: --exclude is set, plan.Removed is non-empty,
// showAffected=false → PrintWarn fires. Asserts the warning
// prefix glyph (the writeStatus "!" convention) AND the
// message body. A refactor that drops either would leave the
// operator without the destructive-subset signal.
func TestPrintPlanText_WarnsOnDestructiveExclude(t *testing.T) {
	// Pin the TTY seam so the glyph assertion is deterministic
	// regardless of sibling-test state (jsonOutput / noColorVal
	// can leak across the gregale package's many tests).
	prevTTY := testOnlyTTY
	prevJSON := jsonOutput
	on := true
	testOnlyTTY = &on
	jsonOutput = false
	t.Cleanup(func() {
		testOnlyTTY = prevTTY
		jsonOutput = prevJSON
	})

	var buf bytes.Buffer
	exit := printPlanText(&buf, basePlan(), []string{"checkout-web"}, false)
	if exit != 0 {
		t.Fatalf("printPlanText exit code: got %d, want 0", exit)
	}
	out := buf.String()
	if !strings.Contains(out, "Applying will soft-delete") {
		t.Fatalf("missing warning body; output:\n%s", out)
	}
	if !strings.Contains(out, "1 app(s)") {
		t.Fatalf("missing count (1) in the warning; output:\n%s", out)
	}
	if !strings.Contains(out, "!") {
		t.Fatalf("missing warning glyph '!'; writeStatus contract broken; output:\n%s", out)
	}
}

// TestPrintPlanText_NoWarnOnShowAffected pins the negative case
// where the operator already opted into the partition view: a
// --show-affected render must NOT add a redundant warning. The
// warn exists to nudge operators who didn't see the partition;
// surfacing the partition + warning would double-up.
func TestPrintPlanText_NoWarnOnShowAffected(t *testing.T) {
	var buf bytes.Buffer
	exit := printPlanText(&buf, basePlan(), []string{"checkout-web"}, true)
	if exit != 0 {
		t.Fatalf("printPlanText exit code: got %d, want 0", exit)
	}
	out := buf.String()
	if strings.Contains(out, "Applying will soft-delete") {
		t.Fatalf("unexpected warning with --show-affected; output:\n%s", out)
	}
}

// TestPrintPlanText_NoWarnWithoutExclude pins the case where
// --exclude is empty: no operator exclude-intent → no warn.
// A non-empty plan.Removed from a scan that simply dropped
// existing apps is not a destructive-exclude scenario; the
// regular partition view surfaces it without a warning.
func TestPrintPlanText_NoWarnWithoutExclude(t *testing.T) {
	plan := basePlan()
	plan.Removed = []string{"checkout-api"}

	var buf bytes.Buffer
	exit := printPlanText(&buf, plan, nil, false)
	if exit != 0 {
		t.Fatalf("printPlanText exit code: got %d, want 0", exit)
	}
	out := buf.String()
	if strings.Contains(out, "Applying will soft-delete") {
		t.Fatalf("unexpected warning without --exclude; output:\n%s", out)
	}
}

// TestPrintPlanText_NoWarnWhenRemovedEmpty pins the case where
// --exclude is set but plan.Removed is empty: the exclude did
// not produce a destructive subset → no warn. The threshold
// is "exclude AND Removed non-empty", not just "exclude set".
func TestPrintPlanText_NoWarnWhenRemovedEmpty(t *testing.T) {
	plan := basePlan()
	plan.Removed = nil // --exclude did not drop any apps

	var buf bytes.Buffer
	exit := printPlanText(&buf, plan, []string{"checkout-web"}, false)
	if exit != 0 {
		t.Fatalf("printPlanText exit code: got %d, want 0", exit)
	}
	out := buf.String()
	if strings.Contains(out, "Applying will soft-delete") {
		t.Fatalf("unexpected warning when Removed is empty; output:\n%s", out)
	}
}

// TestPrintPlanText_WarnCountReflectsPlanRemoved pins the
// warning message's count semantics: it must reflect
// len(plan.Removed), not len(plan.Workloads) or any other
// scalar. A refactor that printed "Applying will soft-delete
// N workloads" using the wrong field would mislead the
// operator about the destructive scale.
func TestPrintPlanText_WarnCountReflectsPlanRemoved(t *testing.T) {
	plan := basePlan()
	plan.Removed = []string{"alpha", "beta", "delta"}

	var buf bytes.Buffer
	exit := printPlanText(&buf, plan, []string{"unrelated"}, false)
	if exit != 0 {
		t.Fatalf("printPlanText exit code: got %d, want 0", exit)
	}
	out := buf.String()
	if !strings.Contains(out, "3 app(s)") {
		t.Fatalf("warning count drift: got output %q, want substring %q", out, "3 app(s)")
	}
}
