// audit_subscriber.go — Issue #472 / ADR-058: bridge the in-process
// `audit_event` pg_notify channel (published by imaged at deploy
// verify-time) into the apid-side pkg/audit writer. imaged emits
// `app.signature_missing`, `app.signature_invalid`, and (the verify-
// success path) `app.signed_image_accepted` via h.notif.Notify(ctx,
// "audit_event", payload) — but imaged is the only fire-and-forget
// publisher on this surface, and there is no other audit-event
// subscriber. Without this bridge the verify-failure events would be
// dropped silently on the floor.
//
// The payload shape is the same JSON apid already emits to itself on
// the in-process path (s.audit.Emit), so the bridge is a thin
// shape-translator. We mirror imaged's payload keys (kind, app_id,
// deployment_id, ref, signer) verbatim into the data map; the audit
// row carries the same JSON column as the existing handler-emitted
// events, so the dashboard doesn't have to special-case "verify
// failure" rows at read-time.
//
// We use SubscribeWithReconnect rather than the bare Subscribe so
// this loop survives Postgres restarts (the same wrapper the
// schedd/deletion_subscriber uses, per pkg/db/notify.go:304). The
// initial Subscribe fails fast at boot if Postgres is unreachable —
// the alternative (silent drop of audit events) is what we're
// closing here, so the boot-time error is the correct posture.
//
// Lifecycle: runAuditSubscriber is invoked once from main.go's
// bgBefore (the production-run closure) and lives for the daemon's
// lifetime. The ctx that cancels the daemon also cancels the
// subscriber.
package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/onebox-faas/faas/pkg/db"
)

// auditEventPayload mirrors the JSON shape `pkg/imaged/handler.go`
// emits via pg_notify('audit_event', payload). The fields are
// server-controlled (imaged writes them; apid reads them); the
// mapping is intentionally narrow — anything outside the named
// fields is dropped at decode time so a future producer-side field
// addition doesn't accidentally fan out into the audit log.
type auditEventPayload struct {
	Kind         string `json:"kind"`
	AppID        string `json:"app_id"`
	DeploymentID string `json:"deployment_id"`
	Ref          string `json:"ref"`
	Signer       string `json:"signer"`
}

// runAuditSubscriber subscribes to db.NotifyAuditEvent and forwards
// each notification to the apid-side *auditor (which delegates to
// the embedded *audit.Auditor for the actual store.AppendEvent
// write). The `pool` is the live pgx.Pool that SubscribeWithReconnect
// holds open; the `a` is the same auditor the request paths
// already use.
//
// The function returns when ctx is cancelled. The initial Subscribe
// error is fatal — the boot-time check is the design and silent
// drop is the bug we're closing.
func runAuditSubscriber(ctx context.Context, pool *pgxpool.Pool, a *auditor, log *slog.Logger) error {
	ch, err := db.SubscribeWithReconnect(ctx, pool, []string{db.NotifyAuditEvent}, log)
	if err != nil {
		return err
	}
	log.Info("audit: subscriber started", "channel", db.NotifyAuditEvent)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case n, ok := <-ch:
			if !ok {
				// Channel closed by the reconnect wrapper on a
				// Postgres restart; the inner loop reconnects, so
				// the outer loop just exits cleanly.
				return nil
			}
			if n.Payload == "" {
				continue
			}
			var p auditEventPayload
			if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
				// Bad payload — log and skip; imaged owns the
				// shape, so a malformed event is a bug, not a
				// customer-facing error.
				log.Warn("audit: bad payload", "channel", n.Channel, "err", err)
				continue
			}
			if p.Kind == "" {
				// Schema pin: kind is required. A nil kind
				// would alias "system" in the audit log; we
				// reject instead.
				log.Warn("audit: empty kind", "channel", n.Channel)
				continue
			}
			data := map[string]any{
				"app_id":        p.AppID,
				"deployment_id": p.DeploymentID,
				"ref":           p.Ref,
				"signer":        p.Signer,
			}
			// app_id is the per-app subject for the audit row.
			// If the publisher didn't include app_id (an
			// older imaged, or a future event with no app
			// scope), subject is nil — the auditor accepts that
			// for system-level events.
			var subject *string
			if p.AppID != "" {
				appID := p.AppID
				subject = &appID
			}
			a.Emit(ctx, p.Kind, subject, data)
		}
	}
}
