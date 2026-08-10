package paddle

// Config is the Paddle on-disk settings. Mirrors stripe.Config —
// every field has a working default so a missing or partial file
// still yields a runnable provider. The loader's LoadBillingConfig
// calls c.Defaults() after the TOML parse so a missing [billing.paddle]
// block in the operator's TOML still yields a usable Config.
type Config struct {
	// APIKey is the Paddle API auth token (pdl_live_… or pdl_sandbox_…).
	// Loaded from FAAS_PADDLE_API_KEY env in cmd/apid and cmd/meterd;
	// empty in dev. Required for live billing operations.
	APIKey string `toml:"api_key"`
	// WebhookSecret is the shared secret Paddle uses to HMAC-sign
	// webhook deliveries. Loaded from FAAS_PADDLE_WEBHOOK_SECRET env.
	// Required only for apid (no webhook ingress in meterd).
	WebhookSecret string `toml:"webhook_secret"`
	// Sandbox toggles api.sandbox.paddle.com vs api.paddle.com.
	// Loaded from FAAS_PADDLE_SANDBOX env ("1" / "true" → true).
	// Default false (production).
	Sandbox bool `toml:"sandbox"`
	// ToleranceSeconds is the replay-protection window applied
	// symmetrically (rejects future-dated timestamps too). Mirrors
	// stripe.Config.ToleranceSeconds. PR-P4 introduced the operator
	// knob; pre-PR-P4 the value was hard-coded to 5*time.Minute in
	// the apid handler. Defaults to webhookDefaultToleranceSeconds
	// (300s, matching Stripe) when <= 0 — the verifier in
	// pkg/billing/paddle/webhook.go clamps to the default at zero
	// so a misconfigured 0 doesn't disable replay protection.
	ToleranceSeconds int `toml:"webhook_tolerance_seconds"`
}

// Defaults fills in working defaults for missing fields. Idempotent —
// a second call leaves populated fields untouched. Kept as a method
// (not inlined into LoadBillingConfig) so a future Paddle field with
// a default can fill in here without churning call sites.
func (c *Config) Defaults() {
	// No-op today. See method doc.
}
