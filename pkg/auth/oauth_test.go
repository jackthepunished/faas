package auth

import (
	"strings"
	"testing"
)

// stubGetenv is a map-backed stand-in for os.Getenv used in tests.
// The constructor defaults all keys to "" so a partial map mirrors
// an unset-envvar environment; explicit deletes are also "".
func stubGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadSignInConfig_BothUnset(t *testing.T) {
	get := stubGetenv(nil)
	cfg, err := LoadSignInConfigFromEnv(get)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Google.Status != SignInProviderDisabled {
		t.Errorf("Google.Status = %v, want Disabled", cfg.Google.Status)
	}
	if cfg.GitHub.Status != SignInProviderDisabled {
		t.Errorf("GitHub.Status = %v, want Disabled", cfg.GitHub.Status)
	}
	if cfg.Google.Enabled() || cfg.GitHub.Enabled() {
		t.Errorf("Enabled should be false for both, got Google=%v GitHub=%v",
			cfg.Google.Enabled(), cfg.GitHub.Enabled())
	}
}

func TestLoadSignInConfig_BothSet(t *testing.T) {
	get := stubGetenv(map[string]string{
		"GOOGLE_CLIENT_ID":     "g-id",
		"GOOGLE_CLIENT_SECRET": "g-sec",
		"GITHUB_CLIENT_ID":     "gh-id",
		"GITHUB_CLIENT_SECRET": "gh-sec",
	})
	cfg, err := LoadSignInConfigFromEnv(get)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Google.Enabled() || cfg.Google.ClientID != "g-id" || cfg.Google.ClientSecret != "g-sec" {
		t.Errorf("Google: %+v", cfg.Google)
	}
	if !cfg.GitHub.Enabled() || cfg.GitHub.ClientID != "gh-id" || cfg.GitHub.ClientSecret != "gh-sec" {
		t.Errorf("GitHub: %+v", cfg.GitHub)
	}
	if !cfg.Enabled(GoogleProviderName) || !cfg.Enabled(GitHubProviderName) {
		t.Errorf("Enabled() lookup broken: %v", cfg)
	}
	if cfg.Enabled("facebook") {
		t.Errorf("unknown provider Enabled() should be false")
	}
}

func TestLoadSignInConfig_GoogleIDOnlyFails(t *testing.T) {
	get := stubGetenv(map[string]string{
		"GOOGLE_CLIENT_ID": "g-id",
		// GOOGLE_CLIENT_SECRET intentionally absent → boot refusal.
	})
	_, err := LoadSignInConfigFromEnv(get)
	if err == nil {
		t.Fatalf("expected half-configured error, got nil")
	}
	for _, want := range []string{"google OAuth", "GOOGLE_CLIENT_ID set", "GOOGLE_CLIENT_SECRET unset", "refusing to boot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

func TestLoadSignInConfig_GoogleSecretOnlyFails(t *testing.T) {
	get := stubGetenv(map[string]string{
		"GOOGLE_CLIENT_SECRET": "g-sec",
	})
	_, err := LoadSignInConfigFromEnv(get)
	if err == nil {
		t.Fatalf("expected half-configured error, got nil")
	}
	for _, want := range []string{"GOOGLE_CLIENT_ID unset", "GOOGLE_CLIENT_SECRET set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

func TestLoadSignInConfig_GitHubIDOnlyFails(t *testing.T) {
	get := stubGetenv(map[string]string{
		"GITHUB_CLIENT_ID": "gh-id",
	})
	_, err := LoadSignInConfigFromEnv(get)
	if err == nil {
		t.Fatalf("expected half-configured error, got nil")
	}
	for _, want := range []string{"GitHub OAuth", "GITHUB_CLIENT_ID set", "GITHUB_CLIENT_SECRET unset"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

func TestLoadSignInConfig_GitHubSecretOnlyFails(t *testing.T) {
	get := stubGetenv(map[string]string{
		"GITHUB_CLIENT_SECRET": "gh-sec",
	})
	_, err := LoadSignInConfigFromEnv(get)
	if err == nil {
		t.Fatalf("expected half-configured error, got nil")
	}
	for _, want := range []string{"GITHUB_CLIENT_ID unset", "GITHUB_CLIENT_SECRET set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing substring %q", err.Error(), want)
		}
	}
}

func TestLoadSignInConfig_MixedConfiguredAndHalfSetFailsFirst(t *testing.T) {
	// GitHub is fully configured; Google is half-set. The loader
	// must fail at the half-set provider even though GitHub would
	// have succeeded in isolation — fail-fast is at the config
	// level, not per-provider.
	get := stubGetenv(map[string]string{
		"GITHUB_CLIENT_ID":     "gh-id",
		"GITHUB_CLIENT_SECRET": "gh-sec",
		"GOOGLE_CLIENT_ID":     "g-id",
	})
	_, err := LoadSignInConfigFromEnv(get)
	if err == nil || !strings.Contains(err.Error(), "google OAuth") {
		t.Fatalf("expected Google half-configured error, got %v", err)
	}
}
