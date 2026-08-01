// event_test.go — tests for the GitHub push-webhook decoder. Pins
// the field names + empty-Before semantics the changed-files filter
// relies on.
package githubd

import (
	"testing"
)

func TestDecodePush_RequiresRequiredFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "happy path: all fields populated",
			body:    `{"ref":"refs/heads/main","before":"abc123","after":"def456","repository":{"full_name":"octo/api"}}`,
			wantErr: false,
		},
		{
			name:    "missing ref",
			body:    `{"before":"abc123","after":"def456","repository":{"full_name":"octo/api"}}`,
			wantErr: true,
		},
		{
			name:    "missing after",
			body:    `{"ref":"refs/heads/main","before":"abc123","repository":{"full_name":"octo/api"}}`,
			wantErr: true,
		},
		{
			name:    "missing repository.full_name",
			body:    `{"ref":"refs/heads/main","before":"abc123","after":"def456","repository":{}}`,
			wantErr: true,
		},
		{
			name:    "empty body",
			body:    ``,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := DecodePush([]byte(tc.body))
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Fatalf("DecodePush() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestDecodePush_BeforeIsExtracted pins the Before field wiring.
// Service.HandlePushRequest reads Before to form the
// compare/{base}...{head} URL; an empty Before (first push on a
// branch) is treated by the caller as the "fall back to full fan-out"
// signal — the decoder itself accepts it.
func TestDecodePush_BeforeIsExtracted(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main","before":"abc123","after":"def456","repository":{"full_name":"octo/api"}}`)
	ev, err := DecodePush(body)
	if err != nil {
		t.Fatalf("DecodePush() err = %v", err)
	}
	if ev.Before != "abc123" {
		t.Errorf("Before = %q, want %q", ev.Before, "abc123")
	}
	if ev.After != "def456" {
		t.Errorf("After = %q, want %q", ev.After, "def456")
	}
	if ev.Ref != "refs/heads/main" {
		t.Errorf("Ref = %q, want %q", ev.Ref, "refs/heads/main")
	}
	if ev.Repository.FullName != "octo/api" {
		t.Errorf("Repository.FullName = %q, want %q", ev.Repository.FullName, "octo/api")
	}
}

// TestDecodePush_EmptyBeforeIsAccepted pins the first-push semantics:
// Before is the only required-ish field the decoder tolerates empty.
// Service.HandlePushRequest falls back to full fan-out when Before
// is empty; the decoder itself does NOT reject (the SHA may be
// 0000...0000 on a fresh branch, which GitHub still emits — but we
// don't want to misclassify an empty/missing field as a config bug
// when GitHub's payload may legitimately omit it).
func TestDecodePush_EmptyBeforeIsAccepted(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ref":"refs/heads/main","after":"def456","repository":{"full_name":"octo/api"}}`)
	ev, err := DecodePush(body)
	if err != nil {
		t.Fatalf("DecodePush() err = %v, want nil (empty Before is tolerated)", err)
	}
	if ev.Before != "" {
		t.Errorf("Before = %q, want empty", ev.Before)
	}
}
