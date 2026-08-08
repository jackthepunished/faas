package stripe

// Config is the stripex on-disk settings. Mirrors schedd/meterd — every
// field has a working default so a missing or partial file still yields
// a runnable daemon.
type Config struct {
	// APIKey is the Stripe secret key (sk_test_… / sk_live_…). Loaded
	// from STRIPE_API_KEY env in cmd/apid and cmd/meterd; empty in dev.
	APIKey string `toml:"api_key"`
	// WebhookSecret is the endpoint signing secret Stripe issues when
	// the webhook endpoint is configured. Loaded from
	// STRIPE_WEBHOOK_SECRET env. The webhook handler refuses payloads
	// without a matching signature.
	WebhookSecret string `toml:"webhook_secret"`
	// Tolerance is the Stripe-Signature timestamp tolerance window.
	// Defaults to 5 min (Stripe's recommended default).
	ToleranceSeconds int `toml:"tolerance_seconds"`
}

// Defaults fills in working defaults for missing fields. Idempotent —
// a second call leaves populated fields untouched. Matches the
// cmd/meterd/config.go:85-87 nested-section shape so the loader's
// LoadBillingConfig can call c.Stripe.Defaults() after the TOML
// parse without special-casing stripe.
//
// 5-minute tolerance mirrors Stripe's recommended default; the
// apid webhook handler passes this value through to
// Client.VerifyWebhook (PR-P2 keeps the existing apid call site
// unchanged — the default is plumbed here, not at the handler).
func (c *Config) Defaults() {
	if c.ToleranceSeconds == 0 {
		c.ToleranceSeconds = 300 // 5 minutes
	}
}
