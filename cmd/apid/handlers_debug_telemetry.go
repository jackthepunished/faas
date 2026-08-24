package main

// handlers_debug_telemetry.go — read-only slice of the production
// debugger (ADR-127) for PR-A. The write-side (publisher → gRPC
// IncrementRequestTelemetry → apid receiver → sqlc INSERT) lands in
// PR-B; PR-A ships the GET endpoint so customers can already see
// the existing app_errors_recorder-style rows once a row source is
// configured.
//
// One handler: GET /v1/apps/{slug}/debug/requests?since=<dur>&limit=N
// The regression / compare / replay endpoints are PR-B / PR-C.

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// debugTelemetryListHandler — GET /v1/apps/{slug}/debug/requests
//
// Plan-gated by DebugTelemetryEnabled. `since` is clamped to the
// plan's DebugTelemetryRetentionDays (Hobby 3d, Pro 7d, Scale 14d).
// `limit` defaults to 20, capped at 200 (matches
// handlers_invocations.go:451-455). The endpoint is IDOR-safe via
// loadApp (cross-account slug → 404).
func (s *server) debugTelemetryListHandler(w http.ResponseWriter, r *http.Request, acct state.Account) {
	app, ok := s.loadApp(w, r, acct, r.PathValue("slug"))
	if !ok {
		return
	}
	limits := api.MustLimitsFor(acct.Plan)
	if !limits.DebugTelemetryEnabled {
		api.WriteProblem(w, api.ErrPlanFeatureGated("debugger", acct.Plan))
		return
	}
	// since is a duration string ("3h", "24h", "7d"); we clamp to
	// the plan's retention cap so a Free user passing ?since=90d
	// is silently rounded down to DebugTelemetryRetentionDays.
	sinceDur := parseDebugSince(r, 24*time.Hour)
	cap := time.Duration(limits.DebugTelemetryRetentionDays) * 24 * time.Hour
	if cap > 0 && sinceDur > cap {
		sinceDur = cap
	}
	limit := 20
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	now := time.Now().UTC()
	rows, err := s.store.ListRequestTelemetryByApp(r.Context(), sqlc.ListRequestTelemetryByAppParams{
		AppID:        stringToPgUUID(app.ID),
		ReceivedAt:   pgtype.Timestamptz{Time: now.Add(-sinceDur), Valid: true},
		ReceivedAt_2: pgtype.Timestamptz{Time: now, Valid: true},
		Limit:        int32(limit),
	})
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("list request telemetry"))
		return
	}
	if rows == nil {
		rows = []sqlc.ListRequestTelemetryByAppRow{}
	}
	items := make([]api.DebugTelemetryRequestItem, len(rows))
	for i, row := range rows {
		items[i] = debugTelemetryRowToItem(row)
	}
	writeJSON(w, http.StatusOK, api.DebugTelemetryListResponse{
		Requests: items,
		Since:    sinceDur.String(),
	})
}

// debugTelemetryRowToItem maps a sqlc-generated row to the wire
// DTO. Lives in cmd/apid/ because pkg/api cannot import pkg/state
// (import cycle). The mapping handles pgtype.UUID → string
// (hyphenated hex), pgtype.Timestamptz → RFC3339Nano, and
// pgtype.Text (nullable trace_id) → *string.
func debugTelemetryRowToItem(row sqlc.ListRequestTelemetryByAppRow) api.DebugTelemetryRequestItem {
	item := api.DebugTelemetryRequestItem{
		// pgtype.UUID -> hyphenated hex string. Falls back to "" when
		// Valid=false so the JSON renders "" rather than the driver's
		// base64 zero-bytes shape.
		ID:           uuidFromPg(row.ID),
		DeploymentID: uuidFromPg(row.DeploymentID),
		Route:        row.Route,
		Method:       row.Method,
		Status:       int(row.Status),
		LatencyMS:    int(row.LatencyMs),
		ColdBoot:     row.ColdBoot,
		ReceivedAt:   timeFromPg(row.ReceivedAt),
	}
	if row.TraceID.Valid {
		s := row.TraceID.String
		item.TraceID = &s
	}
	return item
}

// uuidFromPg renders a pgtype.UUID as the canonical hyphenated-hex
// string. Empty when !Valid so the wire shows "" rather than the
// driver's zero-bytes encoding.
func uuidFromPg(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

// timeFromPg renders a pgtype.Timestamptz as an RFC3339Nano string
// (matches the format the rest of the apid surface uses for
// timestamps). Empty when !Valid.
func timeFromPg(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339Nano)
}

// parseDebugSince parses the ?since= query param. Accepts Go
// duration syntax ("3h", "90m") plus a short alias "Nd" for
// days. Returns def when empty or invalid. Negative durations
// collapse to def so a malicious client can't pull rows from the
// future.
func parseDebugSince(r *http.Request, def time.Duration) time.Duration {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return def
	}
	// Try Go duration first ("3h", "90m", "15s").
	if d, err := time.ParseDuration(raw); err == nil {
		if d <= 0 {
			return def
		}
		return d
	}
	// Try the "Nd" alias.
	if n, err := strconv.Atoi(raw[:len(raw)-1]); err == nil && raw[len(raw)-1] == 'd' && n > 0 {
		return time.Duration(n) * 24 * time.Hour
	}
	return def
}

// stringToPgUUID converts a hyphenated-hex UUID string into the
// pgtype.UUID shape the sqlc-generated queries expect. Invalid
// strings produce a zero-UUID value with Valid=false so the
// Postgres driver returns no rows rather than an error.
func stringToPgUUID(s string) pgtype.UUID {
	uid, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: uid, Valid: true}
}
