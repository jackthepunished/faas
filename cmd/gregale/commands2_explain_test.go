package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestExplainCollector_BucketsByLevel pins the level-bucketing
// contract. Lines split on the first two spaces: ts, level,
// message. info/warn/error are counted; anything else is ignored.
func TestExplainCollector_BucketsByLevel(t *testing.T) {
	c := newExplainCollector("test-app", "deploy-1")
	c.observe("2026-08-18T10:00:00Z info Starting up")
	c.observe("2026-08-18T10:00:01Z warn Slow query")
	c.observe("2026-08-18T10:00:02Z error Connection refused on :8080")
	c.observe("2026-08-18T10:00:03Z error Connection refused on :8080")
	c.observe("2026-08-18T10:00:04Z info Ready")
	if c.infoCount != 2 {
		t.Errorf("expected 2 info, got %d", c.infoCount)
	}
	if c.warnCount != 1 {
		t.Errorf("expected 1 warn, got %d", c.warnCount)
	}
	if c.errorCount != 2 {
		t.Errorf("expected 2 errors, got %d", c.errorCount)
	}
	if !strings.Contains(c.lastError, "Connection refused") {
		t.Errorf("lastError must contain 'Connection refused', got %q", c.lastError)
	}
}

// TestExplainCollector_FlushShape pins the 4-line summary
// contract. Script consumers grep on the literal "error:" /
// "levels:" / "top:" prefixes; changing the shape is a wire
// break for any tool that already parses the summary.
func TestExplainCollector_FlushShape(t *testing.T) {
	c := newExplainCollector("test-app", "deploy-1")
	c.observe("2026-08-18T10:00:00Z error Connection refused")
	c.observe("2026-08-18T10:00:01Z error Connection refused")
	c.observe("2026-08-18T10:00:02Z error Timeout waiting for handshake")
	c.observe("2026-08-18T10:00:03Z info Ready")
	var buf bytes.Buffer
	c.flush(&buf)
	out := buf.String()
	for _, want := range []string{"explain: test-app", "deployment deploy-1", "error:", "levels:", "top:"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "error=3") {
		t.Errorf("expected error=3 in levels line, got:\n%s", out)
	}
	// Pattern coalescing: both "Connection refused" lines collapse
	// into one bucket.
	if strings.Count(out, "Connection refused") < 1 {
		t.Errorf("expected top patterns to include 'Connection refused', got:\n%s", out)
	}
}

// TestExplainCollector_NoErrors pins the "clean run" shape: when
// nothing errors, the summary still emits 4 lines and uses
// "(none)" / "(no error patterns)" so the line count is stable.
func TestExplainCollector_NoErrors(t *testing.T) {
	c := newExplainCollector("test-app", "")
	c.observe("2026-08-18T10:00:00Z info Starting up")
	c.observe("2026-08-18T10:00:01Z info Ready")
	var buf bytes.Buffer
	c.flush(&buf)
	out := buf.String()
	if !strings.Contains(out, "error:  (none)") {
		t.Errorf("expected '(none)' for clean run, got:\n%s", out)
	}
	if !strings.Contains(out, "top:    (no error patterns)") {
		t.Errorf("expected '(no error patterns)', got:\n%s", out)
	}
}

// TestTopPatterns_SortedByCountThenAlphabetical pins the stable
// sort. Top-N output must be deterministic for snapshot consumers;
// ties resolve to alphabetical order, not insertion order.
func TestTopPatterns_SortedByCountThenAlphabetical(t *testing.T) {
	in := map[string]int{
		"alpha":   3,
		"beta":    3,
		"gamma":   2,
		"delta":   5,
		"epsilon": 1,
	}
	out := topPatterns(in, 3)
	want := []string{"delta (N5)", "alpha (N3)", "beta (N3)"}
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d: %v", len(out), out)
	}
	for i, w := range want {
		if out[i] != w {
			t.Errorf("entry %d: want %q, got %q", i, w, out[i])
		}
	}
}

// TestTopPatterns_EmptyInput pins the empty-input contract:
// returns nil so the caller can render "(no error patterns)".
func TestTopPatterns_EmptyInput(t *testing.T) {
	out := topPatterns(map[string]int{}, 3)
	if out != nil {
		t.Errorf("expected nil, got %v", out)
	}
}

// TestExplainCollector_PatternBucketPrefix pins the 64-byte
// prefix bucket contract. The same error message fired 100x with
// timestamps + IDs in the suffix must coalesce into ONE bucket.
func TestExplainCollector_PatternBucketPrefix(t *testing.T) {
	c := newExplainCollector("test-app", "")
	for i := 0; i < 100; i++ {
		c.observe("2026-08-18T10:00:00Z error Connection refused: req=12345 trace=abc")
	}
	if len(c.patterns) != 1 {
		t.Errorf("expected 1 pattern bucket (coalesced by 64-byte prefix), got %d: %v", len(c.patterns), c.patterns)
	}
	for _, v := range c.patterns {
		if v != 100 {
			t.Errorf("expected pattern count 100, got %d", v)
		}
	}
}
