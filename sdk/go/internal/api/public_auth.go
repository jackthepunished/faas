package api

// public_auth.go — per-app public-URL auth mirrors
// (issue #477 / ADR-079). Lives in sdk/go/internal/api
// because the Go SDK at sdk/go is a leaf module with
// only the vendored subset of pkg/api — no
// re-imports back to the daemon's pkg/api (per the
// module-split decision in PR 12). Every type here is
// peer to its counterpart in pkg/api/public_auth.go
// so the SDK sees the same wire shapes.
//
// The constants mirror the closed-enum set:
//
//	"open"   — pre-#477 default; any request passes.
//	"bearer" — Authorization: Bearer <fp_live_…>
//	           with apps:read scope on the owning
//	           account. Hobby+ (Free → 402
//	           plan_public_auth_bearer_not_allowed).
//	"basic"  — Authorization: Basic <user:pass>
//	           with credentials sealed at PATCH time
//	           under the APP_BASIC_AUTH secretbox
//	           namespace. Pro+ (Free/Hobby → 402
//	           plan_public_auth_basic_not_allowed).
//
// Keeping the constants and the JSON tags in sync with
// pkg/api is load-bearing — a drift surfaces as a SDK
// wire-shape mismatch on the next round-trip (the
// gatewayd-internal enforce path is the same set).

// PublicAuthBlock mirrors pkg/api.PublicAuthBlock.
// Mode is the closed-enum string; BasicUser + BasicPass
// are PLAINTEXT at PATCH time (the apid seal step
// encrypts them under the APP_BASIC_AUTH secretbox
// namespace before persistence). For Mode != "basic"
// the basic_user + basic_pass are ignored server-side
// (and any prior sealed blob is cleared).
type PublicAuthBlock struct {
	Mode      string `json:"mode"`
	BasicUser string `json:"basic_user,omitempty"`
	BasicPass string `json:"basic_pass,omitempty"`
}

// PublicAuthStatus mirrors pkg/api.PublicAuthStatus.
// HasBasicCreds is true iff the row carries a
// non-null apps.public_auth_basic blob (mode='basic'
// with credentials). The plaintext basic_user /
// basic_pass are NEVER returned on this surface —
// redaction is enforced at the apid layer
// (ADR-079 §Decision "re-redaction invariant").
type PublicAuthStatus struct {
	Mode          string `json:"mode"`
	HasBasicCreds bool   `json:"has_basic_creds"`
}

// Mode constants — keep in lockstep with
// pkg/api.AppPublicAuthMode* (the daemon's pkg/api is
// the canonical source; SDK mirror is a leaf that
// doesn't import the daemon's pkg/api).
const (
	PublicAuthModeOpen   = "open"
	PublicAuthModeBearer = "bearer"
	PublicAuthModeBasic  = "basic"
)
