package state_test

// PR #1191 follow-up: round-trip the mail suppression surface
// (RecordMailSuppression + IsMailSuppressed) against a real Postgres
// cluster so the (source, provider_event_id) UNIQUE handling, the
// NULL semantics on account_id / expires_at, and the partial-index
// TTL filter all behave as expected. Mirrors pgstore_alert_rules_test.go
// — same pgtest.Open skip-when-no-pg pattern, same package.
//
// The make check-state-coverage gate runs with DATABASE_URL; this file
// is what brings pkg/state coverage back above the 70% floor after the
// mail PR landed.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onebox-faas/faas/pkg/state"
)

// pgSampleMailSuppression is the simplest valid suppression input. A
// fresh uuid on each call so parallel subtests can't replay each other's
// (source, provider_event_id) and accidentally pass the second time
// because of a duplicate-write.
func pgSampleMailSuppression(acctID *string) state.MailSuppressionInput {
	return state.MailSuppressionInput{
		AccountID:       acctID,
		Email:           "user-" + uuid.NewString() + "@example.test",
		Reason:          state.MailSuppressionHardBounce,
		Source:          state.MailSuppressionSourceResend,
		ProviderEventID: "evt-" + uuid.NewString(),
	}
}

// TestPgStore_MailSuppression_RoundTrip exercises the happy path:
// a fresh write returns inserted=true, IsMailSuppressed sees it, and a
// replay with the same (source, provider_event_id) returns inserted=false
// via the ON CONFLICT DO UPDATE arm — the (xmax = 0) discriminator the
// bounce handler reads to decide whether to advance dunning.
func TestPgStore_MailSuppression_RoundTrip(t *testing.T) {
	s, ctx := pgStore(t)
	acct, _, _ := seedLiveDeploy(t, s, ctx)
	acctID := acct

	in := pgSampleMailSuppression(&acctID)
	inserted, err := s.RecordMailSuppression(ctx, in)
	if err != nil {
		t.Fatalf("RecordMailSuppression: %v", err)
	}
	if !inserted {
		t.Errorf("fresh row returned inserted=false; want true")
	}

	suppressed, err := s.IsMailSuppressed(ctx, in.Email)
	if err != nil {
		t.Fatalf("IsMailSuppressed: %v", err)
	}
	if !suppressed {
		t.Errorf("IsMailSuppressed returned false on a freshly written row")
	}

	// Replay must surface inserted=false (the ON CONFLICT DO UPDATE
	// arm fires, xmax != 0). The bounce handler reads this to skip
	// advancing dunning on a redelivery — see the query header in
	// pkg/state/queries.sql for the rationale on choosing ON CONFLICT
	// DO UPDATE over DO NOTHING: it lets us RETURNING the (xmax = 0)
	// discriminator without raising an error to the caller.
	replayed, err := s.RecordMailSuppression(ctx, in)
	if err != nil {
		t.Errorf("replay: unexpected err = %v", err)
	}
	if replayed {
		t.Errorf("replay returned inserted=true; want false (ON CONFLICT arm should fire)")
	}
}

// TestPgStore_MailSuppression_AccountNullable covers the pre-correlation
// bounce case: AccountID==nil still produces a usable suppression row,
// and IsMailSuppressed matches it by email alone.
func TestPgStore_MailSuppression_AccountNullable(t *testing.T) {
	s, ctx := pgStore(t)

	in := pgSampleMailSuppression(nil)
	inserted, err := s.RecordMailSuppression(ctx, in)
	if err != nil {
		t.Fatalf("RecordMailSuppression (account nil): %v", err)
	}
	if !inserted {
		t.Errorf("fresh row with nil account_id returned inserted=false")
	}

	suppressed, err := s.IsMailSuppressed(ctx, in.Email)
	if err != nil {
		t.Fatalf("IsMailSuppressed: %v", err)
	}
	if !suppressed {
		t.Errorf("address with nil account_id not suppressed; want true")
	}
}

// TestPgStore_MailSuppression_TTLFilter pins the partial-index predicate:
// a row whose expires_at is in the past must NOT block future mail,
// because the (lower(email)) WHERE expires_at IS NULL OR expires_at > now()
// index excludes it from the lookup.
func TestPgStore_MailSuppression_TTLFilter(t *testing.T) {
	s, ctx := pgStore(t)

	past := time.Now().Add(-1 * time.Hour)
	in := pgSampleMailSuppression(nil)
	in.ExpiresAt = &past

	if _, err := s.RecordMailSuppression(ctx, in); err != nil {
		t.Fatalf("RecordMailSuppression (past expiry): %v", err)
	}

	suppressed, err := s.IsMailSuppressed(ctx, in.Email)
	if err != nil {
		t.Fatalf("IsMailSuppressed: %v", err)
	}
	if suppressed {
		t.Errorf("expired suppression still active; partial-index TTL filter failed")
	}
}

// TestPgStore_MailSuppression_ValidationErrors proves the four pre-DB
// guards fire before any INSERT, so a malformed payload cannot poison
// the (source, provider_event_id) UNIQUE index with a half-written row.
func TestPgStore_MailSuppression_ValidationErrors(t *testing.T) {
	s, ctx := pgStore(t)

	cases := []struct {
		name    string
		mutate  func(*state.MailSuppressionInput)
		wantErr string
	}{
		{
			name:    "empty email",
			mutate:  func(in *state.MailSuppressionInput) { in.Email = "" },
			wantErr: "email required",
		},
		{
			name:    "empty provider_event_id",
			mutate:  func(in *state.MailSuppressionInput) { in.ProviderEventID = "" },
			wantErr: "provider_event_id required",
		},
		{
			name:    "empty reason",
			mutate:  func(in *state.MailSuppressionInput) { in.Reason = "" },
			wantErr: "reason required",
		},
		{
			name:    "empty source",
			mutate:  func(in *state.MailSuppressionInput) { in.Source = "" },
			wantErr: "source required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := pgSampleMailSuppression(nil)
			tc.mutate(&in)
			_, err := s.RecordMailSuppression(ctx, in)
			if err == nil {
				t.Fatalf("RecordMailSuppression: expected error containing %q, got nil", tc.wantErr)
			}
			if !containsSubstr(err.Error(), tc.wantErr) {
				t.Errorf("RecordMailSuppression err = %q; want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestPgStore_IsMailSuppressed_EmptyEmail pins the input-guard
// regression — an empty email must return a validation error rather
// than scanning the full suppression list.
func TestPgStore_IsMailSuppressed_EmptyEmail(t *testing.T) {
	s, ctx := pgStore(t)

	_, err := s.IsMailSuppressed(ctx, "")
	if err == nil {
		t.Fatalf("IsMailSuppressed(\"\"): expected error, got nil")
	}
	if !containsSubstr(err.Error(), "email required") {
		t.Errorf("IsMailSuppressed err = %q; want substring 'email required'", err.Error())
	}
}

// containsSubstr delegates to strings.Contains so the validation-error
// assertions above can match substrings without pulling in the import
// at every callsite. Renamed from `contains` to avoid clashing with
// the existing pgstore_test.go::contains([]string, string) helper,
// which has a different signature (slice membership vs substring
// match). Lives in this file because only the mail suppression tests
// need substring matching; the rest of the package uses slice
// membership and re-importing strings just to share a 1-line
// polyfill would be more noise than signal.
func containsSubstr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
