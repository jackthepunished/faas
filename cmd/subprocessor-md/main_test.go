package main

import (
	"strings"
	"testing"
	"time"
)

// TestValidate_ValidCatalog pins the 30-day invariant against
// a hand-built catalog. Mirrors the cmd/denylist-md shape (no
// fixture loading in tests; the gate reads the catalog at run()
// time and tests cover the validator directly).
func TestValidate_ValidCatalog(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-02-15" // 45 days after notice — well past the 30-day window
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "postgres-hosting",
				Category:          "database",
				Vendor:            "Hetzner",
				Service:           "Managed single-tenant Postgres",
				DataCategories:    []string{"account metadata", "app metadata"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "Single source of truth",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
			{
				ID:                "stripe-billing",
				Category:          "billing",
				Vendor:            "Stripe",
				Service:           "Stripe Billing",
				DataCategories:    []string{"customer email", "plan tier"},
				DataRegion:        "US",
				DPASigned:         true,
				DPAReference:      "Stripe DPA",
				Rationale:         "Default billing",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	if err := validate(cat); err != nil {
		t.Fatalf("validate() should have accepted a well-formed catalog; got: %v", err)
	}
}

// TestValidate_RejectsShortNoticeWindow pins the negative case:
// effective_date within 30 days of notice_published_at → reject.
func TestValidate_RejectsShortNoticeWindow(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-01-15" // only 14 days after notice
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "test-vendor",
				Category:          "billing",
				Vendor:            "Test",
				Service:           "Test service",
				DataCategories:    []string{"test data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	err := validate(cat)
	if err == nil {
		t.Fatalf("validate() should have rejected a 14-day notice window, got nil")
	}
	if !strings.Contains(err.Error(), "notice window not satisfied") {
		t.Fatalf("error message should mention the notice window, got: %v", err)
	}
}

// TestValidate_AcceptsExactly30DayNotice pins the boundary case:
// exactly 30 days elapsed → accept. Strictly less → reject.
func TestValidate_AcceptsExactly30DayNotice(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-01-31" // exactly 30 days after notice
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "test-vendor",
				Category:          "billing",
				Vendor:            "Test",
				Service:           "Test service",
				DataCategories:    []string{"test data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	if err := validate(cat); err != nil {
		t.Fatalf("30-day boundary case should pass; got: %v", err)
	}
}

// TestValidate_RejectsDuplicateIDs pins the uniqueness invariant.
func TestValidate_RejectsDuplicateIDs(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-02-15"
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "test-vendor",
				Category:          "billing",
				Vendor:            "Test",
				Service:           "Test service A",
				DataCategories:    []string{"test data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
			{
				ID:                "test-vendor",
				Category:          "billing",
				Vendor:            "Test",
				Service:           "Test service B",
				DataCategories:    []string{"test data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	err := validate(cat)
	if err == nil {
		t.Fatalf("validate() should have rejected duplicate ids, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error message should mention duplicate, got: %v", err)
	}
}

// TestValidate_RejectsMissingNoticeDate pins the invariant that any
// sub-processor with an effective_date must have a notice_published_at.
func TestValidate_RejectsMissingNoticeDate(t *testing.T) {
	effective := "2026-02-15"
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:            "test-vendor",
				Category:      "billing",
				Vendor:        "Test",
				Service:       "Test service",
				DataCategories: []string{"test data"},
				DataRegion:    "EU",
				DPASigned:     true,
				DPAReference:  "DPA §7",
				Rationale:     "test",
				EffectiveDate: &effective,
			},
		},
	}
	err := validate(cat)
	if err == nil {
		t.Fatalf("validate() should have rejected missing notice_published_at, got nil")
	}
	if !strings.Contains(err.Error(), "notice_published_at") {
		t.Fatalf("error message should mention notice_published_at, got: %v", err)
	}
}

// TestValidate_RejectsNoticeWindowNot30 pins the gate's hard-coded
// 30-day invariant (DPA §7 — the notice_window_days field must be 30).
func TestValidate_RejectsNoticeWindowNot30(t *testing.T) {
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 14,
		SubProcessors:    []subProcessor{},
	}
	err := validate(cat)
	if err == nil {
		t.Fatalf("validate() should have rejected notice_window_days=14, got nil")
	}
	if !strings.Contains(err.Error(), "notice_window_days must be 30") {
		t.Fatalf("error message should mention the notice_window_days invariant, got: %v", err)
	}
}

// TestRender_DeterministicOrder pins the markdown renderer's
// sort order (by id ascending). The rendered table carries the
// vendor name (the id is only in the JSON), so we sort by id and
// verify the matching vendor names appear in the same order in the
// rendered output. A future refactor that switches to map-iteration
// would fail this test.
func TestRender_DeterministicOrder(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-02-15"
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "zeta-vendor",
				Category:          "billing",
				Vendor:            "Zeta",
				Service:           "Zeta service",
				DataCategories:    []string{"data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
			{
				ID:                "alpha-vendor",
				Category:          "billing",
				Vendor:            "Alpha",
				Service:           "Alpha service",
				DataCategories:    []string{"data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
			{
				ID:                "mu-vendor",
				Category:          "billing",
				Vendor:            "Mu",
				Service:           "Mu service",
				DataCategories:    []string{"data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "DPA §7",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	out1 := render(cat)
	out2 := render(cat)
	if out1 != out2 {
		t.Fatalf("render() is non-deterministic; two calls produced different output")
	}
	// Sort vendors by id ascending (the renderer's order); verify the
	// matching vendor names appear in the same order in the rendered
	// output. The renderer sorts a copy of cat.SubProcessors by id
	// ascending before iterating, so the rendered table row order
	// is alpha → mu → zeta.
	vendorsByOrder := []string{"Alpha", "Mu", "Zeta"}
	prevPos := -1
	for _, vendor := range vendorsByOrder {
		pos := strings.Index(out1, "| billing | "+vendor+" |")
		if pos < 0 {
			t.Errorf("rendered markdown is missing vendor %q row", vendor)
			continue
		}
		if pos <= prevPos {
			t.Errorf("rendered markdown has %q at position %d before previous vendor at %d — sort order regressed", vendor, pos, prevPos)
		}
		prevPos = pos
	}
}

// TestValidate_NoticeWindowDurationCalculation is a pure unit test
// of the date math. Independent of any fixture.
func TestValidate_NoticeWindowDurationCalculation(t *testing.T) {
	notice, _ := time.Parse("2006-01-02", "2026-01-01")
	effective, _ := time.Parse("2006-01-02", "2026-02-15")
	elapsed := effective.Sub(notice)
	if elapsed.Round(24*time.Hour) != 45*24*time.Hour {
		t.Fatalf("expected 45 days elapsed, got %s", elapsed.Round(24*time.Hour))
	}
}

// TestValidate_RejectsDPASignedWithoutReference pins the invariant
// that a sub-processor with dpa_signed=true must also have a
// non-empty dpa_reference.
func TestValidate_RejectsDPASignedWithoutReference(t *testing.T) {
	notice := "2026-01-01"
	effective := "2026-02-15"
	cat := catalog{
		Version:          "test",
		NoticeWindowDays: 30,
		SubProcessors: []subProcessor{
			{
				ID:                "test-vendor",
				Category:          "billing",
				Vendor:            "Test",
				Service:           "Test service",
				DataCategories:    []string{"data"},
				DataRegion:        "EU",
				DPASigned:         true,
				DPAReference:      "",
				Rationale:         "test",
				NoticePublishedAt: &notice,
				EffectiveDate:     &effective,
			},
		},
	}
	err := validate(cat)
	if err == nil {
		t.Fatalf("validate() should have rejected empty dpa_reference with dpa_signed=true")
	}
	if !strings.Contains(err.Error(), "dpa_reference") {
		t.Fatalf("error message should mention dpa_reference, got: %v", err)
	}
}