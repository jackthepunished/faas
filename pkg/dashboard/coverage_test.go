// coverage_test.go — fill the remaining pkg/dashboard coverage gaps
// that the focused render tests (dashboard_test.go) deliberately
// don't touch. Targets:
//
//   - FormatAlertError (dashboard.go:606) — 0.0% → covered. The
//     alert-delivery LastError truncation helper is exposed for
//     cmd/apid/handlers_dashboard.go; a regression in the
//     truncation length would silently let rejected-SSRF URLs
//     blow the panel column width.
//   - RelativeTime (dashboard.go:619) — 0.0% → covered. The
//     coarse "just now / Nm ago / Nh ago / YYYY-MM-DD" formatter
//     is also handler-side and untested. Pin all four branches
//     plus the zero-time + clock-skew paths.
//   - Render (dashboard.go:920) — 73.7% → covered. The remaining
//     uncovered branches:
//       * Empty Body → defaults to "index" (line 922-924)
//       * Template parse/execute failure surfaces (the
//         pre-existing TestRender_MissingTemplate covers Lookup
//         nil; the execute-failure path is hard to inject but
//         the bytes-write failure on w IS reachable via a
//         closed pipe).
//
// Conventions: blackbox `package dashboard_test` (matches the
// pre-existing dashboard_test.go and badge_test.go).

package dashboard_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/onebox-faas/faas/pkg/dashboard"
)

// --- FormatAlertError (dashboard.go:606) ------------------------------

func TestFormatAlertError_ShortStringReturnedUnchanged(t *testing.T) {
	// Under the cap → byte-for-byte echo. Pin the happy path
	// before asserting truncation so a future "always truncate
	// one byte" regression is caught here, not at the boundary.
	for _, s := range []string{
		"",
		"short",
		"http://10.0.0.1:8080/secret",
		strings.Repeat("x", 199), // exactly 1 below the cap
	} {
		if got := dashboard.FormatAlertError(s); got != s {
			t.Errorf("FormatAlertError(%q) = %q, want %q", s, got, s)
		}
	}
}

func TestFormatAlertError_AtBoundaryIsUnchanged(t *testing.T) {
	// len(s) == alertDeliveryErrorLimit (200) → unchanged. The
	// truncation rule fires on >, not >=, so the boundary itself
	// must echo verbatim.
	s := strings.Repeat("x", 200)
	if got := dashboard.FormatAlertError(s); got != s {
		t.Errorf("len=200: got %q, want unchanged", got)
	}
}

func TestFormatAlertError_OverBoundaryTruncated(t *testing.T) {
	// 201 bytes → 199 ASCII bytes + "…" (3 UTF-8 bytes) = 202
	// bytes. Pin the helper's actual wire shape: the byte count
	// is N-1 + len("…") = 199 + 3 = 202. Note this is slightly
	// past the named alertDeliveryErrorLimit constant — the
	// constant names the prefix length, not the total output
	// length. The dashboard column-width assumption tolerates
	// the 2-byte overshoot because the ellipsis collapses
	// visually.
	s := strings.Repeat("x", 201)
	got := dashboard.FormatAlertError(s)
	want := strings.Repeat("x", 199) + "…"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if len(got) != 202 {
		t.Errorf("len = %d, want 202 (199 prefix + 3-byte ellipsis)", len(got))
	}
}

func TestFormatAlertError_FarOverBoundaryTruncated(t *testing.T) {
	// A 32 KiB SSRF-rejected URL → still bounded output after
	// truncation. Pin that the helper doesn't return more than
	// 199 + len("…") bytes regardless of input length.
	s := strings.Repeat("y", 32*1024)
	got := dashboard.FormatAlertError(s)
	if len(got) != 202 {
		t.Errorf("len = %d, want 202 (199 prefix + 3-byte ellipsis)", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("got %q, want '…' suffix", got)
	}
}

// --- RelativeTime (dashboard.go:619) ----------------------------------

func TestRelativeTime_ZeroTimeReturnsDash(t *testing.T) {
	// Pin the "never fired" affordance. The template renders "—"
	// for an absent timestamp; this branch must NOT fall through
	// to "just now" or "future".
	got := dashboard.RelativeTime(time.Time{}, time.Now())
	if got != "—" {
		t.Errorf("got %q, want '—'", got)
	}
}

func TestRelativeTime_JustNow(t *testing.T) {
	// < 1 minute ago → "just now". Includes the 0-second, 30-second,
	// and 59-second cases.
	cases := []time.Duration{
		0,
		30 * time.Second,
		59 * time.Second,
	}
	for _, d := range cases {
		now := time.Now()
		got := dashboard.RelativeTime(now.Add(-d), now)
		if got != "just now" {
			t.Errorf("d=%v: got %q, want 'just now'", d, got)
		}
	}
}

func TestRelativeTime_MinutesAgo(t *testing.T) {
	// [1 minute, 1 hour) → "Nm ago". Pin the boundary cases plus
	// the typical 5-minute / 15-minute / 45-minute reads.
	cases := []struct {
		d        time.Duration
		wantText string
	}{
		{1 * time.Minute, "1m ago"},
		{5 * time.Minute, "5m ago"},
		{15 * time.Minute, "15m ago"},
		{45 * time.Minute, "45m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
	}
	for _, c := range cases {
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		got := dashboard.RelativeTime(now.Add(-c.d), now)
		if got != c.wantText {
			t.Errorf("d=%v: got %q, want %q", c.d, got, c.wantText)
		}
	}
}

func TestRelativeTime_HoursAgo_Under48(t *testing.T) {
	// [1 hour, 48 hours) → "Nh ago". Pin the boundary at exactly
	// 1 hour (rounds down to 1h ago, not 60m ago — pin that the
	// hour formatter takes over from the minute formatter at 1h).
	cases := []struct {
		d        time.Duration
		wantText string
	}{
		{1 * time.Hour, "1h ago"},
		{2 * time.Hour, "2h ago"},
		{12 * time.Hour, "12h ago"},
		{23 * time.Hour, "23h ago"},
		{47*time.Hour + 59*time.Minute, "47h ago"},
	}
	for _, c := range cases {
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		got := dashboard.RelativeTime(now.Add(-c.d), now)
		if got != c.wantText {
			t.Errorf("d=%v: got %q, want %q", c.d, got, c.wantText)
		}
	}
}

func TestRelativeTime_DateFmt_At48h(t *testing.T) {
	// Exactly 48 hours → "YYYY-MM-DD" branch takes over (the
	// "< 48" check is strict). Pin that the boundary itself
	// formats as a date, not "47h ago" / "48h ago".
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-48 * time.Hour)
	got := dashboard.RelativeTime(past, now)
	want := past.UTC().Format("2006-01-02")
	if got != want {
		t.Errorf("d=48h: got %q, want %q (date format)", got, want)
	}
}

func TestRelativeTime_DateFmt_FarPast(t *testing.T) {
	// 1 year ago → date format. Pin the formatter's choice of
	// "2006-01-02" reference layout (no time-of-day, no TZ
	// suffix) — the dashboard's "Last fired" column truncates to
	// date for readability once the gap exceeds 48h.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	past := time.Date(2025, 3, 15, 9, 30, 0, 0, time.UTC)
	got := dashboard.RelativeTime(past, now)
	if got != "2025-03-15" {
		t.Errorf("got %q, want '2025-03-15'", got)
	}
}

func TestRelativeTime_ClockSkewNegativeDiff(t *testing.T) {
	// Pin that negative diffs (future timestamp from clock skew)
	// render as "just now" rather than "<future>" or a negative
	// number. The doc comment specifies this behaviour.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)
	got := dashboard.RelativeTime(future, now)
	if got != "just now" {
		t.Errorf("future timestamp: got %q, want 'just now'", got)
	}
}

// --- Render edge cases (dashboard.go:920) -----------------------------

func TestRender_EmptyBodyDefaultsToIndex(t *testing.T) {
	// Pin the "Body==''" fallback at line 922-924 — Render must
	// coerce to "index" so a future handler that omits Body
	// accidentally still serves a page instead of 404. The
	// existing render tests always set Body explicitly.
	w := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := dashboard.Render(w, log, "nonce-test", dashboard.Page{
		Title: "Empty body",
		// Body deliberately omitted.
		Data: dashboard.IndexData{DeployedAppCount: 0, Plan: "free"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Empty body") {
		t.Errorf("body missing title: %q", body)
	}
	if !strings.Contains(body, "nonce-test") {
		t.Errorf("body missing nonce: %q", body)
	}
}

func TestRender_NonExistentTemplateErrors(t *testing.T) {
	// Pin the Lookup-nil branch at line 931-934. Render returns
	// a wrapped "dashboard: template %q not found" error and
	// MUST NOT write to w. The existing TestRender_MissingTemplate
	// covers this; the assertion that w is NOT written (the
	// missing-template path early-returns before w.Header().Set)
	// is new.
	w := httptest.NewRecorder()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := dashboard.Render(w, log, "", dashboard.Page{Body: "no-such-template-xyz"})
	if err == nil {
		t.Fatal("err = nil, want error for missing template")
	}
	if !strings.Contains(err.Error(), "no-such-template-xyz") {
		t.Errorf("err = %v, want 'no-such-template-xyz' in chain", err)
	}
	if w.Body.Len() != 0 {
		t.Errorf("w.Body = %q, want empty (missing-tpl path must not write)", w.Body.String())
	}
}

func TestRender_BytesWriteFailureBubbles(t *testing.T) {
	// Pin the buf.WriteTo(w) error path at line 946-947. Use a
	// failingResponseWriter that errors on Write so Render's
	// final-stage error surfaces. The headers are set BEFORE the
	// write attempt, so we expect a write error from the body
	// emission.
	fw := &failingResponseWriter{
		headerSink: make(http.Header),
		// Fail after a few bytes so the template has actually
		// produced output but the writer can't drain it all.
		failAfter: 64,
		failErr:   errors.New("client closed connection"),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := dashboard.Render(fw, log, "", dashboard.Page{
		Body: "index",
		Data: dashboard.IndexData{DeployedAppCount: 1, Plan: "hobby"},
	})
	if err == nil {
		t.Fatal("err = nil, want bytes-write error")
	}
	// The exact error message depends on the failingResponseWriter's
	// wiring, but the wrapper must mention the failure mode so a
	// future caller can branch on it.
	if !strings.Contains(err.Error(), "client closed connection") {
		t.Errorf("err = %v, want 'client closed connection' in chain", err)
	}
}

type failingResponseWriter struct {
	headerSink  http.Header
	written     int
	failAfter   int
	failErr     error
	wroteHeader bool
}

func (f *failingResponseWriter) Header() http.Header {
	if f.headerSink == nil {
		f.headerSink = make(http.Header)
	}
	return f.headerSink
}

func (f *failingResponseWriter) Write(p []byte) (int, error) {
	if !f.wroteHeader {
		f.wroteHeader = true
	}
	remaining := f.failAfter - f.written
	if remaining <= 0 {
		return 0, f.failErr
	}
	if len(p) <= remaining {
		f.written += len(p)
		return len(p), nil
	}
	f.written = f.failAfter
	return remaining, f.failErr
}

func (f *failingResponseWriter) WriteHeader(_ int) { f.wroteHeader = true }

// silence unused-warning: bytes.Buffer is referenced in the
// imports for potential future byte-assertion tests.
var _ = bytes.NewBuffer
