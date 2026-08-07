package main

import (
	"testing"
	"time"
)

func TestParseDeadline_DurationString(t *testing.T) {
	before := time.Now()
	got, err := parseDeadline("24h")
	if err != nil {
		t.Fatalf("parseDeadline(24h): %v", err)
	}
	delta := time.Until(got)
	// 24h minus the time spent in this test = roughly 24h. Allow
	// 1s slack on either side for clock granularity.
	if delta < 24*time.Hour-time.Second || delta > 24*time.Hour+time.Second {
		t.Errorf("parseDeadline(24h) = %v from now, want ~24h (got delta %v since %v)",
			got, delta, before)
	}
}

func TestParseDeadline_RFC3339(t *testing.T) {
	now := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	got, err := parseDeadline(now)
	if err != nil {
		t.Fatalf("parseDeadline(RFC3339): %v", err)
	}
	if d := time.Until(got); d < time.Hour || d > 3*time.Hour {
		t.Errorf("parseDeadline(RFC3339) delta = %v, want ~2h", d)
	}
}

func TestParseDeadline_EmptyFallsBackToDefault(t *testing.T) {
	before := time.Now()
	got, err := parseDeadline("")
	if err != nil {
		t.Fatalf("parseDeadline(\"\"): %v", err)
	}
	delta := time.Until(got)
	if delta < defaultSessionDeadline-time.Second || delta > defaultSessionDeadline+time.Second {
		t.Errorf("parseDeadline(\"\") = %v, want ~%v from now (got delta %v since %v)",
			got, defaultSessionDeadline, delta, before)
	}
}

func TestParseDeadline_Garbage(t *testing.T) {
	if _, err := parseDeadline("not-a-time"); err == nil {
		t.Errorf("parseDeadline(\"not-a-time\") should fail, got nil")
	}
}

func TestParseDeadline_NegativeDuration(t *testing.T) {
	if _, err := parseDeadline("-1h"); err == nil {
		t.Errorf("parseDeadline(\"-1h\") must reject negative durations (would produce a past-time deadline and 502 every request)")
	}
}

func TestParseDeadline_PastTimestamp(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := parseDeadline(past); err == nil {
		t.Errorf("parseDeadline(<past RFC3339>) must reject timestamps in the past")
	}
}
