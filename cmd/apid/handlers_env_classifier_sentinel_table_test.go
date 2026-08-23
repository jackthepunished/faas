// Issue #957 — typed-sentinel unit test.
//
// handlers_env.go::runEnvClassifier returns *errEnvClassifier
// sentinels at every failure site; setEnv's audit emit uses
// errors.As to read the Kind discriminator and stamp the
// silent_skip boolean for host_hash_failed.
//
// The integration seam (TestSetEnv_ClassifierFailure_HostHashFailed_
// EmitsAuditEvent) covers only the silent-skip branch — the
// remaining branches (uuid_parse, port_out_of_range,
// classifier_internal, insert_data_upstream) require pgtest
// (state.MemStore's data_upstreams methods are Postgres-only
// stubs; see pkg/state/memstore_data_upstreams.go:33).
//
// This file pins the table directly: each sentinel's Kind +
// Unwrap target is asserted against a fixed expected value so a
// future refactor that drifts the closed-vocab trips the gate
// silently otherwise.

package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrEnvClassifier_Sentinels(t *testing.T) {
	cases := []struct {
		name           string
		sentinel       *errEnvClassifier
		wantKind       string
		wantSilentSkip bool // silent_skip = (Kind == "host_hash_failed")
	}{
		{
			name:           "uuid_parse",
			sentinel:       errClassifierUUIDParse,
			wantKind:       "uuid_parse",
			wantSilentSkip: false,
		},
		{
			name:           "host_hash_failed",
			sentinel:       errClassifierHostHashFailed,
			wantKind:       "host_hash_failed",
			wantSilentSkip: true,
		},
		{
			name:           "insert_data_upstream",
			sentinel:       errClassifierInsert,
			wantKind:       "insert_data_upstream",
			wantSilentSkip: false,
		},
		{
			name:           "port_out_of_range",
			sentinel:       errClassifierPortRange,
			wantKind:       "port_out_of_range",
			wantSilentSkip: false,
		},
		{
			name:           "classifier_internal",
			sentinel:       errClassifierInternal,
			wantKind:       "classifier_internal",
			wantSilentSkip: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sentinel == nil {
				t.Fatalf("sentinel %s is nil", tc.name)
			}
			if tc.sentinel.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", tc.sentinel.Kind, tc.wantKind)
			}
			if tc.sentinel.Error() == "" {
				t.Errorf("Error() = empty string")
			}
			// errors.As on the sentinel must succeed against
			// the *errEnvClassifier target.
			var ec *errEnvClassifier
			if !errors.As(tc.sentinel, &ec) {
				t.Errorf("errors.As(sentinel, &*errEnvClassifier) = false")
			}
			if ec.Kind != tc.wantKind {
				t.Errorf("after As: ec.Kind = %q, want %q", ec.Kind, tc.wantKind)
			}
			// silent_skip dispatch mirrors setEnv's audit emit.
			gotSilentSkip := ec.Kind == errClassifierHostHashFailed.Kind
			if gotSilentSkip != tc.wantSilentSkip {
				t.Errorf("silent_skip dispatch = %v, want %v (Kind=%q)",
					gotSilentSkip, tc.wantSilentSkip, ec.Kind)
			}
		})
	}
}

// TestErrEnvClassifier_WrapWithInner checks that wrapping an inner
// error preserves Unwrap() and that errors.As(target) surfaces
// the inner. Used at the runEnvClassifier sites that already had a
// concrete cause (e.g. uuid.Parse, sql.ErrConnDone).
func TestErrEnvClassifier_WrapWithInner(t *testing.T) {
	inner := errors.New("pgx: conn closed")
	wrapped := &errEnvClassifier{
		Kind: errClassifierInternal.Kind,
		Err:  inner,
	}
	if !errors.Is(wrapped, inner) {
		t.Errorf("errors.Is(wrapped, inner) = false; Unwrap() = %v", wrapped.Unwrap())
	}
	if wrapped.Kind != "classifier_internal" {
		t.Errorf("Kind = %q, want classifier_internal", wrapped.Kind)
	}
	wantMsg := fmt.Sprintf("env-classifier: classifier_internal: %s", inner.Error())
	if wrapped.Error() != wantMsg {
		t.Errorf("Error() = %q, want %q", wrapped.Error(), wantMsg)
	}
	// errors.As should find the inner.
	if !errors.Is(wrapped, inner) {
		t.Errorf("errors.Is(wrapped, inner) = false")
	}
}
