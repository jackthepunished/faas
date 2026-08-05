package main

// `gregale webhooks <list|add|update|rm|deliveries|retry> ...`
// (issue #476 / ADR-076). Mirrors the crons dispatcher at
// commands2.go::cmdCrons but is split into its own file because the
// surface has more subcommands (six vs four) and the retry-policy
// closed-set drift test lives next to the dispatcher that surfaces
// it. Pattern: every leaf hits apid via authedClient(); tabular
// output goes through the same fmt.Printf rows as crons so the
// column widths match what `gregale crons list` already produces.
//
// Subcommand vocab mirrors the api/openapi.yaml kebab-case:
//   list    GET /v1/apps/{slug}/webhooks
//   add     POST /v1/apps/{slug}/webhooks
//   update  PATCH /v1/apps/{slug}/webhooks/{id}
//   rm      DELETE /v1/apps/{slug}/webhooks/{id}
//   deliveries GET /v1/apps/{slug}/webhooks/{id}/deliveries
//   retry   POST /v1/apps/{slug}/webhooks/{id}/deliveries/{did}/retry

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/onebox-faas/faas/pkg/api"
)

func cmdWebhooks(args []string) int {
	if len(args) == 0 {
		PrintUsage(os.Stderr, "usage: gregale webhooks <list|add|update|rm|deliveries|retry> [args]", "webhooks")
		return 1
	}
	switch args[0] {
	case subList:
		return cmdWebhooksList(args[1:])
	case subAdd:
		return cmdWebhooksAdd(args[1:])
	case subUpdate:
		return cmdWebhooksUpdate(args[1:])
	case subRm:
		return cmdWebhooksRm(args[1:])
	case "deliveries":
		return cmdWebhookDeliveries(args[1:])
	case "retry":
		return cmdWebhookRetry(args[1:])
	}
	fmt.Fprintf(os.Stderr, "unknown webhooks subcommand %q\n", args[0])
	return 1
}

func cmdWebhooksList(args []string) int {
	fs := flag.NewFlagSet("webhooks-list", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" {
		PrintUsage(os.Stderr, "usage: gregale webhooks list --app <slug>", "webhooks")
		return 1
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	out, err := client.ListAppWebhooks(context.Background(), *slug)
	if err != nil {
		return printErr("Request failed", err)
	}
	if jsonOutput {
		return jsonOut(writeNDJSON(out))
	}
	for _, w := range out {
		fmt.Printf("%-32s %-50s %-10s %s\n",
			w.ID, truncate(w.TargetURL, 50), w.RetryPolicy, enabledStr(w.Enabled))
	}
	return 0
}

func cmdWebhooksAdd(args []string) int {
	fs := flag.NewFlagSet("webhooks-add", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	target := fs.String("target-url", "", "HTTPS target URL (required)")
	secret := fs.String("secret", "", "HMAC-SHA256 secret (optional; auto-minted if empty)")
	var events multiFlag
	fs.Var(&events, "event", "event name (repeat for multiple); empty = all events")
	policy := fs.String("retry-policy", "default", "retry policy: default|aggressive|none")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || *target == "" {
		PrintUsage(os.Stderr, "usage: gregale webhooks add --app <slug> --target-url <url> [--event <evt>]... [--retry-policy default|aggressive|none] [--secret <hmac-secret>]", "webhooks")
		return 1
	}
	// Closed-set drift test BEFORE the round-trip — same posture as
	// the --eviction-priority check in cmdApp (PR #647). Surfaces a
	// typo locally instead of letting apid return 400 app_webhook_invalid
	// after the network call.
	switch *policy {
	case "default", "aggressive", "none":
	default:
		return printErr("Invalid --retry-policy", fmt.Errorf("must be 'default', 'aggressive', or 'none'; got %q", *policy))
	}
	for _, ev := range events {
		if !validAppWebhookEvent(ev) {
			return printErr("Invalid --event", fmt.Errorf("unknown event %q (allowed: cron.fired, app.deployed, app.scaled, app.parked, app.woken)", ev))
		}
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	req := api.CreateAppWebhookRequest{
		TargetURL:   *target,
		EventFilter: events,
		RetryPolicy: *policy,
	}
	if *secret != "" {
		req.WebhookSecret = *secret
	}
	out, err := client.CreateAppWebhook(context.Background(), *slug, req)
	if err != nil {
		return printErr("Create failed", err)
	}
	PrintOK(osStdout, "Webhook subscribed: %s -> %s (id=%s)", out.ID, out.TargetURL)
	return 0
}

func cmdWebhooksUpdate(args []string) int {
	fs := flag.NewFlagSet("webhooks-update", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	target := fs.String("target-url", "", "new target URL")
	policy := fs.String("retry-policy", "", "new retry policy (default|aggressive|none)")
	enable := fs.Bool("enable", false, "enable")
	disable := fs.Bool("disable", false, "disable")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || len(fs.Args()) == 0 {
		PrintUsage(os.Stderr, "usage: gregale webhooks update <id> --app <slug> [--target-url X] [--retry-policy X] [--enable|--disable]", "webhooks")
		return 1
	}
	id := fs.Args()[0]
	if *enable && *disable {
		return printErr("Invalid flags", fmt.Errorf("--enable and --disable are mutually exclusive"))
	}
	switch *policy {
	case "", "default", "aggressive", "none":
	default:
		return printErr("Invalid --retry-policy", fmt.Errorf("must be 'default', 'aggressive', or 'none'; got %q", *policy))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	req := api.UpdateAppWebhookRequest{}
	if *target != "" {
		req.TargetURL = &*target
	}
	if *policy != "" {
		req.RetryPolicy = &*policy
	}
	if *enable {
		t := true
		req.Enabled = &t
	} else if *disable {
		f := false
		req.Enabled = &f
	}
	out, err := client.UpdateAppWebhook(context.Background(), *slug, id, req)
	if err != nil {
		return printErr("Update failed", err)
	}
	PrintOK(osStdout, "Updated webhook %s", out.ID)
	return 0
}

func cmdWebhooksRm(args []string) int {
	fs := flag.NewFlagSet("webhooks-rm", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || len(fs.Args()) != 1 {
		PrintUsage(os.Stderr, "usage: gregale webhooks rm --app <slug> <id>", "webhooks")
		return 1
	}
	id := fs.Args()[0]
	if !webhookIDPattern.MatchString(id) {
		return printErr("Invalid webhook id", fmt.Errorf("must be a 32-hex-char UUID; got %q", id))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	if err := client.DeleteAppWebhook(context.Background(), *slug, id); err != nil {
		return printErr("Delete failed", err)
	}
	PrintOK(osStdout, "Removed")
	return 0
}

func cmdWebhookDeliveries(args []string) int {
	fs := flag.NewFlagSet("webhooks-deliveries", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	status := fs.String("status", "", "filter by status (pending|in_flight|succeeded|failed|dead)")
	pageSize := fs.Int("page-size", 50, "page size (1..100)")
	pageToken := fs.String("page-token", "", "opaque cursor from previous call")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || len(fs.Args()) == 0 {
		PrintUsage(os.Stderr, "usage: gregale webhooks deliveries --app <slug> <id> [--status X] [--page-size N] [--page-token T]", "webhooks")
		return 1
	}
	id := fs.Args()[0]
	switch *status {
	case "", "pending", "in_flight", "succeeded", "failed", "dead":
	default:
		return printErr("Invalid --status", fmt.Errorf("must be one of pending|in_flight|succeeded|failed|dead; got %q", *status))
	}
	if *pageSize < 1 || *pageSize > 100 {
		return printErr("Invalid --page-size", fmt.Errorf("must be in [1,100]; got %d", *pageSize))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	out, err := client.ListAppWebhookDeliveries(context.Background(), *slug, id, api.ListAppWebhookDeliveriesOptions{
		Status:    *status,
		PageSize:  *pageSize,
		PageToken: *pageToken,
	})
	if err != nil {
		return printErr("Request failed", err)
	}
	if jsonOutput {
		return jsonOut(writeNDJSON(out.Deliveries))
	}
	for _, d := range out.Deliveries {
		fmt.Printf("%-32s %-10s attempt=%-2d status=%-10s code=%-4d %s\n",
			d.ID, d.Event, d.Attempt, d.Status,
			d.LastResponseCode, truncate(d.LastError, 60))
	}
	if out.NextToken != "" {
		fmt.Fprintf(os.Stderr, "next page: --page-token %s\n", out.NextToken)
	}
	return 0
}

func cmdWebhookRetry(args []string) int {
	fs := flag.NewFlagSet("webhooks-retry", flag.ContinueOnError)
	slug := fs.String("app", "", "app slug (required)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *slug == "" || len(fs.Args()) != 2 {
		PrintUsage(os.Stderr, "usage: gregale webhooks retry --app <slug> <webhook-id> <delivery-id>", "webhooks")
		return 1
	}
	webhookID, deliveryID := fs.Args()[0], fs.Args()[1]
	if !webhookIDPattern.MatchString(webhookID) {
		return printErr("Invalid webhook id", fmt.Errorf("must be a 32-hex-char UUID; got %q", webhookID))
	}
	client, err := authedClient()
	if err != nil {
		return printErr("Not logged in", err)
	}
	out, err := client.RetryAppWebhookDelivery(context.Background(), *slug, webhookID, deliveryID)
	if err != nil {
		return printErr("Retry failed", err)
	}
	PrintOK(os.Stdout, "Queued for retry: delivery=%s status=%s next_attempt_at=%s",
		out.Delivery.ID, out.Delivery.Status, out.Delivery.NextAttemptAt)
	return 0
}

// webhookIDPattern matches the 32-hex shape apid uses for webhook
// ids. Same convention as deploymentIDPattern / cronIDPattern.
var webhookIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// validAppWebhookEvents is the closed vocabulary accepted by the
// --event flag. Mirrors the CHECK constraint at
// migrations/00141_app_webhook_deliveries.sql:73-77 and the
// `app_webhook_deliveries_event_chk` test in
// 00141_app_webhook_deliveries_test.go:148-170.
var validAppWebhookEvents = map[string]struct{}{
	"cron.fired":   {},
	"app.deployed": {},
	"app.scaled":   {},
	"app.parked":   {},
	"app.woken":    {},
}

func validAppWebhookEvent(s string) bool {
	_, ok := validAppWebhookEvents[s]
	return ok
}

func enabledStr(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// multiFlag is a flag.Value that accumulates repeated occurrences.
// Mirrors the same pattern in cmd/gregale/commands2.go's flag.Var
// usage for crons; lets `--event cron.fired --event app.deployed`
// build a 2-element slice without quoting tricks. Empty values are
// skipped so callers can omit the flag entirely.
type multiFlag []string

func (m *multiFlag) String() string {
	if m == nil {
		return ""
	}
	return strings.Join(*m, ",")
}

func (m *multiFlag) Set(v string) error {
	if v == "" {
		return nil
	}
	*m = append(*m, v)
	return nil
}
