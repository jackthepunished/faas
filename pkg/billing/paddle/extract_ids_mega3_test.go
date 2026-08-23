// extract_ids_mega3_test.go — Coverage Mega-PR #3 cluster C:
// fill the remaining extractIDs branch gap that
// coverage_sweep2_test.go leaves open, plus the trivial
// Config.Defaults no-op that the loader relies on for the partial
// TOML case (config.go:36).
//
// extractIDs at 64.3% on the baseline because coverage_sweep2 only
// exercises the subscription happy path (which short-circuits on
// the first json.Unmarshal). The transaction-fallback branch is
// reached only when the subscription unmarshal fails — i.e. the
// `items` field is present but in a shape that the subscription
// struct cannot decode.
//
// Config.Defaults at 0% on the baseline because no test calls it.
// The method is currently a no-op (per the doc), but the loader
// invokes it post-TOML-parse and a future field with a working
// default will land here. The idem-potency contract is the
// load-bearing invariant: a second call must leave populated
// fields untouched (per config.go:32-37).
//
// Whitebox `package paddle`.

package paddle

import (
	"encoding/json"
	"testing"
)

// TestExtractIDs_TransactionFallback covers the second
// json.Unmarshal branch at webhook.go:103-106 — reached only when
// the subscription struct cannot decode the payload (e.g. `items`
// is a string instead of an array).
func TestExtractIDs_TransactionFallback(t *testing.T) {
	t.Parallel()
	body := json.RawMessage(`{
		"customer_id": "ctm_fallback",
		"subscription_id": "sub_fallback",
		"items": "not-an-array"
	}`)
	c, s, p := extractIDs(body)
	if c != "ctm_fallback" {
		t.Errorf("customer = %q, want ctm_fallback", c)
	}
	if s != "sub_fallback" {
		t.Errorf("subscription = %q, want sub_fallback", s)
	}
	if p != "" {
		t.Errorf("plan = %q, want empty (txn branch has no items)", p)
	}
}

// TestExtractIDs_BothUnmarshalsFail covers the silent-failure
// path at webhook.go:89 + webhook.go:103 — neither subscription
// nor txn struct can decode the payload.
func TestExtractIDs_BothUnmarshalsFail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body json.RawMessage
	}{
		{"string payload", json.RawMessage(`"hello"`)},
		{"number payload", json.RawMessage(`42`)},
		{"bool payload", json.RawMessage(`true`)},
		{"null payload", json.RawMessage(`null`)},
		{"array payload", json.RawMessage(`[1,2,3]`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc, ss, pp := extractIDs(c.body)
			if cc != "" || ss != "" || pp != "" {
				t.Errorf("got = (%q,%q,%q); want all empty for %s payload %s",
					cc, ss, pp, c.name, string(c.body))
			}
		})
	}
}

// TestExtractIDs_TxnShapeWithExtraItems confirms the
// subscription-struct's empty-items path returns all fields set
// when items:null is given.
func TestExtractIDs_TxnShapeWithExtraItems(t *testing.T) {
	t.Parallel()
	body := json.RawMessage(`{
		"customer_id": "ctm_txn_extra",
		"subscription_id": "sub_txn_extra",
		"items": null
	}`)
	c, s, p := extractIDs(body)
	if c != "ctm_txn_extra" {
		t.Errorf("customer = %q, want ctm_txn_extra", c)
	}
	if s != "sub_txn_extra" {
		t.Errorf("subscription = %q, want sub_txn_extra", s)
	}
	if p != "" {
		t.Errorf("plan = %q, want empty (items:null → no price id)", p)
	}
}

// TestConfigDefaults_Idempotent pins the contract at
// config.go:32-37: Defaults must leave populated fields untouched
// (a second call is a no-op).
func TestConfigDefaults_Idempotent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "zero-valued Config remains zero",
			in:   Config{},
			want: Config{},
		},
		{
			name: "populated fields remain populated",
			in: Config{
				APIKey:           "pdl_live_abc123",
				WebhookSecret:    "whsec_xyz",
				Sandbox:          true,
				ToleranceSeconds: 600,
			},
			want: Config{
				APIKey:           "pdl_live_abc123",
				WebhookSecret:    "whsec_xyz",
				Sandbox:          true,
				ToleranceSeconds: 600,
			},
		},
		{
			name: "second Defaults call is a no-op",
			in:   Config{APIKey: "k", ToleranceSeconds: 120},
			want: Config{APIKey: "k", ToleranceSeconds: 120},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.in
			c.Defaults()
			if c != tc.want {
				t.Errorf("after Defaults: %+v, want %+v", c, tc.want)
			}
			c.Defaults()
			if c != tc.want {
				t.Errorf("after 2x Defaults: %+v, want %+v", c, tc.want)
			}
		})
	}
}
