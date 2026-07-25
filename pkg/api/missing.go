package api

import "context"

// This file holds SDK methods for endpoints the CLI doesn't (yet)
// wrap but the spec exposes. The list below is the complete diff
// between the OpenAPI routes and the methods declared in
// pkg/api/client.go — every entry here is reachable via
// `pkg/api.Client.<Method>` even though `faas <subcommand>` doesn't
// invoke it today.
//
// As of 2026-07-25 the only entry is UsageSummary (no `faas usage
// summary` subcommand yet — `faas usage` calls GetUsage, the
// per-app rows). When the CLI wraps an endpoint, MOVE its method
// from here to client.go and delete the doc block — that's how this
// file stays a useful inventory rather than a graveyard.
//
// Adding a new endpoint to api/openapi.yaml? Add a typed method here
// first; the make sdk-check drift gate (commit 3) catches the case
// where someone ships a route without a method.

// UsageSummary returns the account-wide monthly roll-up
// (used_gb_hours, included_gb_hours, overage_gb_hours, overage_cents).
// Distinct from GetUsage which returns per-app rows.
func (c *Client) UsageSummary(ctx context.Context, month string) (UsageSummaryResponse, error) {
	var out UsageSummaryResponse
	path := "/v1/usage/summary"
	if month != "" {
		path += "?month=" + month
	}
	return out, c.do(ctx, "GET", path, nil, &out)
}
