// coverage_test.go — fill the remaining pkg/dashboard/stages coverage
// gaps that stages_test.go deliberately doesn't touch. Targets:
//
//   - htmlEscape (stages.go:479) — 54.5% → covered. Pin all five
//     HTML-text metacharacter branches plus the default pass-through.
//     The "failed: <reason>" footer renders into template.HTML;
//     a regression here breaks the SLO panel for any reason
//     containing & / < / > / " / '.
//   - assertClosedSet (stages.go:196) — 50% → covered. The function
//     runs at every RenderSummaryText/RenderSummaryHTML call; pin
//     the happy path (no drift).
//   - RenderSummaryText edge cases — failed stage with HTML-meta
//     reason (text path doesn't escape; HTML path does), live /
//     supersede footer variants, mid-flight partial completion.
//   - RenderSummaryHTML edge cases — supersede footer, live footer,
//     HTML-escape of failed reason, partial completion.

package stages

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

// --- htmlEscape (stages.go:479) ---------------------------------------

func TestHtmlEscape_AllMetachars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"hello", "hello"},
		{"a & b", "a &amp; b"},
		{"<script>", "&lt;script&gt;"},
		{`"quoted"`, "&quot;quoted&quot;"},
		{"it's", "it&#39;s"},
		{"a&b<c>d\"e'f", "a&amp;b&lt;c&gt;d&quot;e&#39;f"},
		// Unicode pass-through (default branch).
		{"héllo wörld", "héllo wörld"},
		// CRLF preserved.
		{"line1\r\nline2", "line1\r\nline2"},
		// Empty after escape.
		{"&", "&amp;"},
		{"<", "&lt;"},
		{">", "&gt;"},
		{`"`, "&quot;"},
		{"'", "&#39;"},
	}
	for _, c := range cases {
		if got := htmlEscape(c.in); got != c.want {
			t.Errorf("htmlEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- assertClosedSet (stages.go:196) -----------------------------------

func TestAssertClosedSet_Happy(t *testing.T) {
	// No panic in steady state — drift triggers panic, but the
	// happy path is what every RenderSummary* call exercises.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("assertClosedSet panicked in happy path: %v", r)
		}
	}()
	assertClosedSet()
}

// --- RenderSummaryText edge cases ------------------------------------

func TestRenderSummaryText_PartialCompletion(t *testing.T) {
	// Three stages completed, three not yet started. Pin that
	// partial completion renders all 6 rows with the canonical
	// "pending" affordance for the missing ones, and the footer
	// is suppressed when terminalAt is zero (Review finding C3).
	now := time.Now().UTC()
	started := now.Add(-30 * time.Second)
	ended := now.Add(-29 * time.Second)
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[0], StartedAt: &started, EndedAt: &ended, DurationMs: 1000, Status: "completed"},
			{Name: StageOrder()[2], StartedAt: &started, EndedAt: &ended, DurationMs: 5000, Status: "completed"},
			{Name: StageOrder()[4], StartedAt: &started, EndedAt: &ended, DurationMs: 12000, Status: "completed"},
		},
	}
	var buf bytes.Buffer
	if err := RenderSummaryText(&buf, ss, "live", time.Time{}); err != nil {
		t.Fatalf("RenderSummaryText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Source downloaded", "Dependencies restored", "Image built", "Security scan", "Snapshot prepared", "Readiness passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing label %q: %s", want, out)
		}
	}
	if strings.Contains(out, "live since") {
		t.Errorf("footer should be suppressed with zero terminalAt: %s", out)
	}
}

func TestRenderSummaryText_FailedStageFooter(t *testing.T) {
	// One stage failed with a reason containing HTML metacharacters.
	// The TEXT path passes the reason verbatim (no escape — the
	// renderer writes plain bytes to w). The HTML path escapes.
	// Pin both contracts here.
	now := time.Now().UTC()
	terminal := now.Add(-5 * time.Second)
	started := now.Add(-30 * time.Second)
	ended := now.Add(-29 * time.Second)
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[0], StartedAt: &started, EndedAt: &ended, DurationMs: 1000, Status: "completed"},
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 100, Status: "failed", Reason: "OOM <512MB>"},
		},
	}
	var buf bytes.Buffer
	if err := RenderSummaryText(&buf, ss, "failed", terminal); err != nil {
		t.Fatalf("RenderSummaryText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "OOM <512MB>") {
		t.Errorf("text path should pass reason verbatim (no escape): %s", out)
	}
	if !strings.Contains(out, "failed at") {
		t.Errorf("failed footer missing: %s", out)
	}
}

func TestRenderSummaryText_LiveFooter(t *testing.T) {
	now := time.Now().UTC()
	terminal := now.Add(-2 * time.Minute)
	started := terminal
	ended := terminal
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 0, Status: "completed"},
		},
	}
	var buf bytes.Buffer
	if err := RenderSummaryText(&buf, ss, "live", terminal); err != nil {
		t.Fatalf("RenderSummaryText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "live since") {
		t.Errorf("live footer missing: %s", out)
	}
}

func TestRenderSummaryText_SupersedeFooter(t *testing.T) {
	now := time.Now().UTC()
	terminal := now.Add(-1 * time.Minute)
	started := terminal
	ended := terminal
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 0, Status: "completed"},
		},
	}
	var buf bytes.Buffer
	if err := RenderSummaryText(&buf, ss, "superseded", terminal); err != nil {
		t.Fatalf("RenderSummaryText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "superseded at") {
		t.Errorf("supersede footer missing: %s", out)
	}
}

func TestRenderSummaryText_FooterWriteErrors(t *testing.T) {
	// Failure modes in the footer fmt.Fprintf paths. Use a writer
	// that fails after N bytes so the row writes succeed but the
	// "Total:" / "<status> at:" footers fail. Pin the wrapped
	// error path at stages.go:335 / 340.
	now := time.Now().UTC()
	started := now.Add(-30 * time.Second)
	ended := now.Add(-29 * time.Second)
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 1000, Status: "completed"},
		},
	}
	// Small fail budget so footer writes start failing midway.
	ew := &errAfterWriter{allow: 250, err: errors.New("disk full")}
	err := RenderSummaryText(ew, ss, "live", now)
	if err == nil {
		t.Fatal("err = nil, want footer write error")
	}
}

type errAfterWriter struct {
	allow int
	err   error
	wrote int
}

func (e *errAfterWriter) Write(p []byte) (int, error) {
	remaining := e.allow - e.wrote
	if remaining <= 0 {
		return 0, e.err
	}
	if len(p) <= remaining {
		e.wrote += len(p)
		return len(p), nil
	}
	e.wrote = e.allow
	return remaining, e.err
}

// --- RenderSummaryHTML edge cases ------------------------------------

func TestRenderSummaryHTML_FailedReasonIsHTMLEscaped(t *testing.T) {
	// The HTML path MUST escape the failed reason so a hostile
	// imaged-side message ("<script>alert(1)</script>") can't
	// break out of the <span class="duration"> wrapper.
	now := time.Now().UTC()
	started := now.Add(-30 * time.Second)
	ended := now.Add(-29 * time.Second)
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 100, Status: "failed", Reason: `<script>alert(1)</script>`},
		},
	}
	got := RenderSummaryHTML(ss, "failed", now)
	out := string(got)
	if strings.Contains(out, "<script>alert") {
		t.Errorf("HTML path leaked unescaped reason: %s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("HTML escape not applied to reason: %s", out)
	}
}

func TestRenderSummaryHTML_SupersedeFooter(t *testing.T) {
	now := time.Now().UTC()
	terminal := now.Add(-1 * time.Minute)
	started := terminal
	ended := terminal
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 0, Status: "completed"},
		},
	}
	got := string(RenderSummaryHTML(ss, "superseded", terminal))
	if !strings.Contains(got, "superseded at") {
		t.Errorf("HTML supersede footer missing: %s", got)
	}
}

func TestRenderSummaryHTML_LiveFooter(t *testing.T) {
	now := time.Now().UTC()
	terminal := now.Add(-2 * time.Minute)
	started := terminal
	ended := terminal
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[5], StartedAt: &started, EndedAt: &ended, DurationMs: 0, Status: "completed"},
		},
	}
	got := string(RenderSummaryHTML(ss, "live", terminal))
	if !strings.Contains(got, "live since") {
		t.Errorf("HTML live footer missing: %s", got)
	}
}

func TestRenderSummaryHTML_PartialCompletion(t *testing.T) {
	// Two stages completed; four pending. All 6 labels must still
	// render — partial completion is a normal mid-flight state.
	now := time.Now().UTC()
	started := now.Add(-30 * time.Second)
	ended := now.Add(-29 * time.Second)
	ss := state.StageState{
		History: []state.StageStateItem{
			{Name: StageOrder()[0], StartedAt: &started, EndedAt: &ended, DurationMs: 1000, Status: "completed"},
			{Name: StageOrder()[2], StartedAt: &started, EndedAt: &ended, DurationMs: 5000, Status: "completed"},
		},
	}
	got := string(RenderSummaryHTML(ss, "live", time.Time{}))
	for _, want := range []string{"Source downloaded", "Dependencies restored", "Image built", "Security scan", "Snapshot prepared", "Readiness passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing label %q: %s", want, got)
		}
	}
	// Footer must be suppressed when terminalAt is zero AND
	// total > 0 (we have 6000ms of completed work) — the gate
	// is "footer IF totalMs > 0 AND terminalAt != 0".
	if strings.Contains(got, "live since") {
		t.Errorf("footer should be suppressed with zero terminalAt: %s", got)
	}
}
