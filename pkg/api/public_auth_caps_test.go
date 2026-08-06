package api

// public_auth_caps_test.go — pin the size caps on the
// basic-auth payload. The seal step uses
// AppPublicAuthBasicMaxBytes as the upper bound; the
// Validate() method uses the per-field caps. If a future
// contributor diverges the per-field sum from the
// total-cap, the seal step could accept a multi-megabyte
// payload past the per-field Validate gate. Mirrors the
// webhook-secret cap-pair shape (pkg/api/webhooks.go).
//
// Lives in the internal package because the constants
// (AppPublicAuthBasicMaxBytes, AppPublicAuthBasicUserMaxBytes,
// AppPublicAuthBasicPassMaxBytes) are unexported — the
// cross-package constant-agreement test is in
// public_auth_constants_test.go (package api_test).

import "testing"

// TestPublicAuthBasicCapsBounded pins the per-field ↔ total
// cap relationship. The seal step (cmd/apid/handlers_ext.go)
// builds the plaintext as "<user>\n<pass>" before sealing,
// so the total-cap must be at least user_max + 1 + pass_max.
// A future contributor tightening per-field caps without
// updating the total-cap would surface as a 422 the
// validator let through; the inverse (loosening per-field
// but leaving the total) would surface as a 503 from the
// seal step. Both branches fail here.
func TestPublicAuthBasicCapsBounded(t *testing.T) {
	want := AppPublicAuthBasicUserMaxBytes + 1 + AppPublicAuthBasicPassMaxBytes
	if AppPublicAuthBasicMaxBytes < want {
		t.Errorf("AppPublicAuthBasicMaxBytes = %d, want >= %d (user + \\n + pass)",
			AppPublicAuthBasicMaxBytes, want)
	}
	if AppPublicAuthBasicUserMaxBytes < 1 {
		t.Errorf("AppPublicAuthBasicUserMaxBytes = %d, want >= 1", AppPublicAuthBasicUserMaxBytes)
	}
	if AppPublicAuthBasicPassMaxBytes < 1 {
		t.Errorf("AppPublicAuthBasicPassMaxBytes = %d, want >= 1", AppPublicAuthBasicPassMaxBytes)
	}
}
