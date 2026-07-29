// Package auth — sign-in OAuth provider configuration (issue #419).
//
// The sign-in OAuth flow lives in cmd/apid/handlers_google.go and
// cmd/apid/handlers_github.go. Before #419 those handlers read
// `os.Getenv("GOOGLE_CLIENT_ID")` etc. at request time, which meant an
// apid host missing the env vars would boot clean (`/healthz` returned
// `{"status":"ok"}`) and only surface the misconfiguration as a 500
// `*_oauth_misconfigured` when a real customer clicked the OAuth
// button. Worse: passwordless accounts (those who only ever signed up
// via OAuth) cannot log in via POST /login by design — the §11
// anti-enumeration contract (pkg/auth.DummyPHC) returns 401
// invalid_credentials for them. With both consent routes dead, those
// accounts had no entry point at all.
//
// This file lands the boot-time validation that closes the gap. The
// design (ADR-046) is a hybrid:
//
//   - Both ID and SECRET present   → SignInProviderConfigured
//   - Both unset                   → SignInProviderDisabled (warn + 503
//     on click)
//   - Exactly one of ID / SECRET set → LoadSignInConfigFromEnv returns a
//     wrapped error and apid refuses to boot. This is the half-set
//     footgun where the consent redirect succeeds against an unfilled
//     client_id and the callback then 500s on the missing secret —
//     better to surface the typo at boot than to ship the silent
//     fallback.
//
// The package is named `auth` (not `pkg/auth/oauth`) so callers do
// `auth.SignInConfig` — the existing `api.OAuthProvider` (string enum
// for the OAuth wire-shape) lives in pkg/api and would collide with
// any `auth.OAuthProvider` struct; we keep the boundary explicit.
package auth

import "fmt"

// SignInProviderStatus is the resolved state of one sign-in OAuth
// provider. The closed set {Configured, Disabled} is intentional —
// there is no third option (the half-set case fails the load, see
// LoadSignInConfigFromEnv).
type SignInProviderStatus int

const (
	// SignInProviderConfigured means both the ID and the SECRET for
	// the provider are present in the process environment. The
	// handler may build a consent redirect URL.
	SignInProviderConfigured SignInProviderStatus = iota + 1

	// SignInProviderDisabled means neither ID nor SECRET is set —
	// the operator chose not to ship this provider on this host.
	// The handler returns 503 oauth_provider_unavailable on click;
	// the dashboard hides the button (templates/login.html
	// {{if .Auth.<Provider>Enabled}} guards).
	SignInProviderDisabled
)

// SignInProvider is one configured OAuth provider. ClientID,
// ClientSecret, and RedirectURI are empty when Status == Disabled.
// Callers that need the credentials MUST guard on `Enabled()` first
// so Disabled cases can't accidentally read empties into a redirect
// URL. RedirectURI is the optional host-derived override read from
// `GOOGLE_REDIRECT_URI` / `GITHUB_REDIRECT_URI`; empty means "derive
// from the request's Host header" (the historic behaviour the
// handlers fall back to).
type SignInProvider struct {
	Status       SignInProviderStatus
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// Enabled reports whether the provider's consent-redirect path may
// build a URL. False when Status == Disabled; also false when
// ClientID or ClientSecret is empty even with Status ==
// Configured (defence in depth — boot validation refuses to start
// on a half-set config, but a future direct constructor call or
// a test-only SignInConfig must not bypass the guard).
func (p SignInProvider) Enabled() bool {
	return p.Status == SignInProviderConfigured &&
		p.ClientID != "" && p.ClientSecret != ""
}

// SignInConfig is the resolved sign-in OAuth surface for one
// apid process. It is computed once at boot (cmd/apid/main.go::runWithDeps)
// and read everywhere (handlers, `/v1/auth/capabilities`, dashboard
// template, `/healthz` extension if we add one later).
//
// The struct intentionally separates "Google" and "GitHub" — even
// though the wire-shape `api.OAuthProvider` enum has a closed set,
// this struct is the BOOT-TIME resolved state of those providers
// and is not interchangeable with the wire enum.
type SignInConfig struct {
	Google SignInProvider
	GitHub SignInProvider
}

// Provider-name constants used by SignInConfig.Enabled and the
// route handlers (issue #419 / ADR-046). Keep the closed set here
// so the Capabilities response shape and the `apid_oauth_disabled_total`
// label values share one source of truth. Unknown names passed to
// SignInConfig.Enabled return false — the only sanctioned names
// are these constants.
const (
	GoogleProviderName = "google"
	GitHubProviderName = "github"
)

// Enabled reports whether the named provider may build a consent
// redirect URL. name must be GoogleProviderName or GitHubProviderName;
// unknown names return false.
func (c SignInConfig) Enabled(name string) bool {
	switch name {
	case GoogleProviderName:
		return c.Google.Enabled()
	case GitHubProviderName:
		return c.GitHub.Enabled()
	default:
		return false
	}
}

// LoadSignInConfigFromEnv resolves the sign-in OAuth provider state
// by reading the four documented env vars through the supplied
// getter:
//
//   - GOOGLE_CLIENT_ID + GOOGLE_CLIENT_SECRET
//   - GITHUB_CLIENT_ID + GITHUB_CLIENT_SECRET
//
// The getter is injected so cmd/apid/main.go can pass os.Getenv
// while tests pass a map-backed stub. The getter MUST return ""
// for unset keys (matching os.Getenv's behaviour); a key that is
// not in the map counts as unset.
//
// Error semantics:
//
//   - Both unset per provider → Disabled (no error).
//   - Both set per provider   → Configured (no error).
//   - Exactly one of (ID, SECRET) set per provider → non-nil,
//     wrapped error mentioning both var names. apid's runWithDeps
//     propagates this and refuses to boot (fail-fast; ADR-046).
//
// The half-set case is detected for both Google and GitHub; the
// returned error names which provider tripped the footgun so the
// operator acts on the right line.
func LoadSignInConfigFromEnv(getenv func(string) string) (SignInConfig, error) {
	cfg := SignInConfig{}

	gID := getenv("GOOGLE_CLIENT_ID")
	gSec := getenv("GOOGLE_CLIENT_SECRET")
	gRedirect := getenv("GOOGLE_REDIRECT_URI")
	switch {
	case gID == "" && gSec == "":
		cfg.Google = SignInProvider{Status: SignInProviderDisabled}
	case gID != "" && gSec != "":
		cfg.Google = SignInProvider{
			Status:       SignInProviderConfigured,
			ClientID:     gID,
			ClientSecret: gSec,
			RedirectURI:  gRedirect,
		}
	default:
		return SignInConfig{}, fmt.Errorf("google OAuth is half-configured: %s set but %s %s; both must be set or both unset (issue #419, ADR-046)",
			whichSet("GOOGLE_CLIENT_ID", gID),
			whichSet("GOOGLE_CLIENT_SECRET", gSec),
			bootFootnote())
	}

	ghID := getenv("GITHUB_CLIENT_ID")
	ghSec := getenv("GITHUB_CLIENT_SECRET")
	ghRedirect := getenv("GITHUB_REDIRECT_URI")
	switch {
	case ghID == "" && ghSec == "":
		cfg.GitHub = SignInProvider{Status: SignInProviderDisabled}
	case ghID != "" && ghSec != "":
		cfg.GitHub = SignInProvider{
			Status:       SignInProviderConfigured,
			ClientID:     ghID,
			ClientSecret: ghSec,
			RedirectURI:  ghRedirect,
		}
	default:
		return SignInConfig{}, fmt.Errorf("GitHub OAuth is half-configured: %s set but %s %s; both must be set or both unset (issue #419, ADR-046)",
			whichSet("GITHUB_CLIENT_ID", ghID),
			whichSet("GITHUB_CLIENT_SECRET", ghSec),
			bootFootnote())
	}

	return cfg, nil
}

// whichSet returns name if value is non-empty, "name unset" otherwise.
// Used purely to assemble the half-configured error message so the
// operator sees which two variables diverge.
func whichSet(name, value string) string {
	if value == "" {
		return name + " unset"
	}
	return name + " set"
}

// bootFootnote is the trailing tail of the half-configured error,
// pointing the operator at the boot-fail-fast behaviour. Centralised
// so a wording tweak is one line.
func bootFootnote() string {
	return "; refusing to boot — set both, unset both, or do not start apid"
}
