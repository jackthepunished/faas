package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// TestRenderWakeTimelinePage_TriggersAndContext pins the ADR-123
// human output shape: every wake.boot_started / wake.boot_completed
// row gains a trailing `trigger=… q=N c=N` line; the page header
// gains a `triggers: foo=N bar=M` histogram. Stable ordering across
// runs (sort.Strings) keeps the golden-file check simple.
func TestRenderWakeTimelinePage_TriggersAndContext(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	resp := api.WakeTimelineResponse{
		WakeID: "wake-1",
		AppID:  "app-1",
		Limit:  10,
		Events: []api.WakeTimelineEvent{
			{At: now, Kind: "wake.boot_started", Actor: "schedd",
				Data: map[string]any{
					"trigger":              "gateway",
					"queued_count":         float64(3),
					"concurrency_at_admit": float64(2),
				}},
			{At: now, Kind: "wake.boot_completed", Actor: "schedd",
				Data: map[string]any{
					"trigger":              "gateway",
					"queued_count":         float64(3),
					"concurrency_at_admit": float64(2),
				}},
		},
	}
	var buf bytes.Buffer
	renderWakeTimelinePage(&buf, resp)
	out := buf.String()
	for _, want := range []string{
		"wake wake-1 app app-1 limit 10:",
		"triggers: gateway=1", // summary aggregates by wake.boot_started
		"trigger=gateway q=3 c=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestRenderSummaryHeader_SortedByKey pins the deterministic key
// order (sort.Strings) so the CLI output is grep-able / golden-friendly.
func TestRenderSummaryHeader_SortedByKey(t *testing.T) {
	events := []api.WakeTimelineEvent{
		{Kind: "wake.boot_started", Data: map[string]any{"trigger": "scaleup"}},
		{Kind: "wake.boot_started", Data: map[string]any{"trigger": "gateway"}},
		{Kind: "wake.boot_started", Data: map[string]any{"trigger": "cron.schedule"}},
		{Kind: "wake.boot_completed", Data: map[string]any{"trigger": "ignored"}},
	}
	var buf bytes.Buffer
	renderSummaryHeader(&buf, events)
	got := buf.String()
	want := "  triggers: cron.schedule=1 gateway=1 scaleup=1\n"
	if got != want {
		t.Errorf("renderSummaryHeader = %q, want %q", got, want)
	}
}

// TestRenderSummaryHeader_AbsentNoop verifies the histogram is
// suppressed when no wake.boot_started event carries a trigger
// (pre-ADR-123 fleet or events emitted by an older schedd).
func TestRenderSummaryHeader_AbsentNoop(t *testing.T) {
	events := []api.WakeTimelineEvent{
		{Kind: "wake.boot_completed", Data: map[string]any{}},
	}
	var buf bytes.Buffer
	renderSummaryHeader(&buf, events)
	if buf.Len() != 0 {
		t.Errorf("expected no header line, got %q", buf.String())
	}
}

// TestRenderContextSuffix_LegacyEvent pins the legacy event shape:
// a wake.boot_started without ADR-123 fields renders no trailing
// context line (so the human output stays byte-identical for
// pre-ADR-123 events).
func TestRenderContextSuffix_LegacyEvent(t *testing.T) {
	ev := api.WakeTimelineEvent{Kind: "wake.boot_started", Data: map[string]any{}}
	if got := renderContextSuffix(ev); got != "" {
		t.Errorf("legacy event rendered context %q, want \"\"", got)
	}
}

// TestRenderContextSuffix_TriggerOnly covers the cron branch:
// trigger stamped, but queue/concurrency both zero (cron-driven
// cold boot has no waiting-requests and no siblings).
func TestRenderContextSuffix_TriggerOnly(t *testing.T) {
	ev := api.WakeTimelineEvent{
		Kind: "wake.boot_started",
		Data: map[string]any{"trigger": "cron.schedule"},
	}
	got := renderContextSuffix(ev)
	if got != "trigger=cron.schedule" {
		t.Errorf("renderContextSuffix = %q, want %q", got, "trigger=cron.schedule")
	}
}
