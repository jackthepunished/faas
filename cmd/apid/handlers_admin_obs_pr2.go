// Operator observability backend — PR #2 (issue #777 / ADR-091 §3.5 + §3.6).
//
// Endpoints added in this file:
//
//	GET /v1/admin/obs/anomalies      — hour-of-day baseline aggregate
//	GET /v1/admin/obs/rate-limits    — durable + live snapshot
//
// Both routes inherit the §3.1 two-layer gate (admin scope +
// FAAS_ADMIN_EMAILS allowlist) and §3.2 MFA requirement. The rate-
// limits endpoint is the only surface that touches an in-process
// auth limiter; the live snapshot is intentionally apid-local
// (gatewayd-public does NOT publish rate-limit state — see ADR-091
// §3.5 "No inter-process state for the live view").
//
// The anomalies endpoint is read-only against usage_minutes; no
// rate-limit-bucket writes, no event-table inserts. The scoring
// formula is in pkg/state/queries.sql::TrafficAnomalyAggregate
// and pinned there (sqlc is the canonical source per ADR-017).
package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/middleware"
	"github.com/onebox-faas/faas/pkg/state"
	"github.com/onebox-faas/faas/pkg/state/sqlc"
)

// obsAnomalyBaselineDays is fixed at 7 per ADR-091 §3.6 ("baseline =
// hour-of-day ... over the last 7 days"). The constant is exposed
// here rather than in pkg/api/limits.go because the baseline window
// is a model parameter, not a cap; it is not exposed as a query
// parameter on the wire. Future PRs that let the operator widen
// the baseline (e.g. on multi-host) move this constant to limits.go.
const obsAnomalyBaselineDays = 7

// obsAnomalySinceDefault / obsAnomalySinceMax bound the ?window_hours
// query parameter. The default matches the rate-limit / overview
// 24h convention so a tile on the operator dashboard doesn't need
// to re-tune per endpoint. The max is the §3.7 ObsAdminWindowMaxHours
// cap (168h = 7d).
const (
	obsAnomalySinceDefault = 24 * time.Hour
	obsAnomalySinceMax     = time.Duration(api.ObsAdminWindowMaxHours) * time.Hour
)

// obsRateLimitSinceDefault / obsRateLimitSinceMax mirror the anomaly
// window bounds — same shape, same rationale.
const (
	obsRateLimitSinceDefault = 24 * time.Hour
	obsRateLimitSinceMax     = time.Duration(api.ObsAdminWindowMaxHours) * time.Hour
)

// obsRateLimitLiveLagSeconds is the documented durable-view lag
// (ADR-091 §3.5). The operator UI surfaces it in a tooltip on the
// durable table; the value is informational and intentionally NOT
// computed from aggregator state — the durable view's flush
// cadence is 30s and we do not want a runtime dependency on
// meterd's clock from the apid HTTP path.
const obsRateLimitLiveLagSeconds = 30

// obsRateLimitSourceDurable / obsRateLimitSourceLive are the
// wire-stable source names for the rate-limit aggregate (ADR-091
// §3.5). They are name-spaced from the deployment-status literal
// "live" (handlers_admin_obs_projection.go) because goconst
// matches literal text, not resource. Adding a future source
// (gatewayd-public rate-limit snapshot) is an append — the
// Sources slice stays stable.
const (
	obsRateLimitSourceDurable = "durable"
	obsRateLimitSourceLive    = "live"
)

// obsAnomalies handles GET /v1/admin/obs/anomalies (ADR-091 §3.6).
//
// Query parameters:
//
//	?window_hours=<n>   default 24, cap 168 (ObsAdminWindowMaxHours)
//	?limit=<n>          default 50 (ObsAdminAnomalyLimitDefault),
//	                    cap 200 (ObsAdminAnomalyLimitMax)
//
// Response is a single ObsAnomalyListResponse object — items are
// pre-sorted server-side by z_score DESC so the operator UI can
// render the table without re-sorting. Items is always non-nil so
// the JSON shape is stable on empty windows.
func (s *server) obsAnomalies(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if allowed, prob := s.adminAllows(acct); !allowed {
		api.WriteProblem(w, prob)
		return
	}
	prob, window := parseObsWindowHours(r.URL.Query().Get("window_hours"),
		obsAnomalySinceDefault, obsAnomalySinceMax, "anomalies")
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	prob, limit := api.ParseLimit(r.URL.Query().Get("limit"),
		api.ObsAdminAnomalyLimitDefault, api.ObsAdminAnomalyLimitMax, "anomalies")
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	since := time.Now().UTC().Add(-window)
	baselineCutoff := since.AddDate(0, 0, -obsAnomalyBaselineDays)
	rows, err := s.store.TrafficAnomalyAggregate(r.Context(),
		sqlc.TrafficAnomalyAggregateParams{
			Minute:   pgtype.Timestamptz{Time: since, Valid: true},
			Minute_2: pgtype.Timestamptz{Time: baselineCutoff, Valid: true},
			Column3:  int64(limit),
		})
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not query anomalies"))
		return
	}
	items := make([]api.ObsAnomalyRow, 0, len(rows))
	for _, row := range rows {
		items = append(items, toObsAnomalyRow(row))
	}
	writeJSON(w, http.StatusOK, api.ObsAnomalyListResponse{
		GeneratedAt:        time.Now().UTC(),
		WindowHours:        int(window / time.Hour),
		BaselineWindowDays: obsAnomalyBaselineDays,
		Items:              items,
	})
}

// obsRateLimits handles GET /v1/admin/obs/rate-limits (ADR-091 §3.5).
//
// Query parameters:
//
//	?window_hours=<n>     default 24, cap 168
//	?limit=<n>            default 100 (ObsAdminRateLimitLimitDefault),
//	                      cap 500 (ObsAdminRateLimitLimitMax)
//	?account_id=<uuid>    (planned for PR #3) — narrows the durable
//	                      view to one account. PR #2 ignores it so a
//	                      pre-existing operator UI doesn't silently
//	                      start returning unfiltered data.
//
// The live view comes from s.apiAuthLimiter.Snapshot() — the
// shared per-IP bucket every /v1/* route draws from. The snapshot
// is a deep copy (pkg/middleware/authlimit.go) so this handler
// does not hold the limiter mutex during JSON marshalling.
//
// Sources is always ["durable", "live"] today; future additions
// (gatewayd-public rate-limit snapshot, multi-host aggregator)
// appear here without breaking the wire contract.
func (s *server) obsRateLimits(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if allowed, prob := s.adminAllows(acct); !allowed {
		api.WriteProblem(w, prob)
		return
	}
	prob, window := parseObsWindowHours(r.URL.Query().Get("window_hours"),
		obsRateLimitSinceDefault, obsRateLimitSinceMax, "rate-limits")
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	prob, limit := api.ParseLimit(r.URL.Query().Get("limit"),
		api.ObsAdminRateLimitLimitDefault, api.ObsAdminRateLimitLimitMax, "rate-limits")
	if prob != nil {
		api.WriteProblem(w, prob)
		return
	}
	since := time.Now().UTC().Add(-window)
	rows, err := s.store.PerAccountRateLimitAggregate(r.Context(),
		sqlc.PerAccountRateLimitAggregateParams{
			At:      pgtype.Timestamptz{Time: since, Valid: true},
			Column2: int64(limit),
		})
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not query rate-limit aggregate"))
		return
	}
	durable := make([]api.ObsRateLimitDurableRow, 0, len(rows))
	for _, row := range rows {
		durable = append(durable, toObsRateLimitDurableRow(row))
	}
	live := toObsRateLimitLiveRows(s.apiAuthLimiter)
	writeJSON(w, http.StatusOK, api.ObsRateLimitResponse{
		GeneratedAt: time.Now().UTC(),
		WindowHours: int(window / time.Hour),
		Sources:     []string{obsRateLimitSourceDurable, obsRateLimitSourceLive},
		LagSeconds:  obsRateLimitLiveLagSeconds,
		Durable:     durable,
		Live:        live,
	})
}

// parseObsWindowHours parses ?window_hours=<n> into a time.Duration.
// Default + max are passed in so a single helper handles both the
// anomalies and rate-limits endpoints with identical semantics.
// Returns a *api.Problem on out-of-range input; the handler writes
// the 400 with the canonical "limit+observed" detail.
func parseObsWindowHours(raw string, def, max time.Duration, label string) (*api.Problem, time.Duration) {
	if raw == "" {
		return nil, def
	}
	var hours int
	if _, err := fmt.Sscanf(raw, "%d", &hours); err != nil || hours <= 0 {
		return api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid window", fmt.Sprintf("?window_hours=%q is not a positive integer", raw)).
			WithDocs("https://docs.faas.example/admin/obs"), 0
	}
	maxHours := int(max / time.Hour)
	if hours > maxHours {
		return api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Window too large", fmt.Sprintf("?window_hours=%d exceeds the %d-hour cap for %s", hours, maxHours, label)).
			WithLimit(int64(maxHours), int64(hours)).
			WithDocs("https://docs.faas.example/admin/obs"), 0
	}
	return nil, time.Duration(hours) * time.Hour
}

// toObsAnomalyRow projects a sqlc row onto the wire DTO. AccountID
// and AppID are converted from pgtype.UUID to the canonical UUID
// string the rest of the API uses. Minute is rendered as RFC 3339
// to match the other time-series endpoints on /v1/admin/obs/*.
func toObsAnomalyRow(row sqlc.TrafficAnomalyAggregateRow) api.ObsAnomalyRow {
	var z *float64
	if row.ZScore != nil {
		if f, ok := row.ZScore.(*float64); ok && f != nil {
			z = f
		}
	}
	return api.ObsAnomalyRow{
		AccountID:       uuidString(row.AccountID),
		AppID:           uuidString(row.AppID),
		Minute:          row.Minute.Time.UTC().Format(time.RFC3339),
		Current:         row.CurrentMbSeconds,
		BaselineMean:    row.MeanMbSeconds,
		BaselineStddev:  row.StddevMbSeconds,
		BaselineSamples: int(row.SampleCount),
		ZScore:          z,
		Reason:          row.Reason,
	}
}

// toObsRateLimitDurableRow projects the durable aggregate row onto
// the wire DTO. LastEventAt is sqlc's interface{} (its MAX(at)
// returns timestamptz); the helper type-asserts and falls back to
// time.Time{} on a nil value (defensive — MAX never returns NULL
// over a non-empty window, but the interface{} shape doesn't
// promise that).
func toObsRateLimitDurableRow(row sqlc.PerAccountRateLimitAggregateRow) api.ObsRateLimitDurableRow {
	last := time.Time{}
	if t, ok := row.LastEventAt.(time.Time); ok {
		last = t
	}
	return api.ObsRateLimitDurableRow{
		AccountID:   uuidString(row.AccountID),
		Hits:        int(row.Hits),
		LastEventAt: last.UTC(),
	}
}

// toObsRateLimitLiveRows projects the authLimiter snapshot onto the
// wire DTO list. CurrentlyRateLimited is true when the IP's hits
// count meets or exceeds the bucket's MaxFailures (the limiter
// would 429 the next request). LastEventAt is the most recent
// failure timestamp inside the bucket window — same shape as the
// durable view's last_event_at so the operator UI can render a
// unified timeline.
//
// Nil-safe: a nil limiter (unit tests that don't wire the
// pipeline) returns an empty slice, not nil.
func toObsRateLimitLiveRows(lim *middleware.Limiter) []api.ObsRateLimitLiveRow {
	if lim == nil {
		return []api.ObsRateLimitLiveRow{}
	}
	snap := lim.Snapshot()
	out := make([]api.ObsRateLimitLiveRow, 0, len(snap.Entries))
	for _, e := range snap.Entries {
		out = append(out, api.ObsRateLimitLiveRow{
			IP:                   e.IP,
			CurrentlyRateLimited: e.Hits >= snap.MaxFailures,
			LiveHits30s:          e.Hits,
			LastEventAt:          snap.Now,
		})
	}
	return out
}

// uuidString renders a pgtype.UUID as a canonical UUID string via
// its String() method. pgtype.UUID.String() returns "" when Valid
// is false — the operator UI can branch on the field's presence
// rather than its content. The all-zeros UUID sentinel
// (00000000-0000-0000-0000-000000000000) for the anonymous
// rate-limit bucket is rendered verbatim.
func uuidString(u pgtype.UUID) string {
	return u.String()
}
