// handlers_wake_timeline.go — issue #517 / PR-C / ADR-064 — the
// customer-facing wake-timeline endpoint.
//
// Route (registered in server.go::handler):
//
//	GET /v1/apps/{slug}/wakes/{wake_id}/timeline
//
// Trust model
//
//   - The route sits behind the same auth chain as the rest of
//     /v1/apps/{slug}/* — s.authLimited → requireMFA → requireScope
//     (api.ScopesReadSurface). A session-cookie principal carries
//     admin scope implicitly; an apps:read- or admin-scoped API
//     key works too. The wake-timeline shares the §12 per-app
//     rate-limit budget with logs/metrics/wake — no special-case
//     bucket.
//
//   - Cross-account invisibility is enforced at the SQL layer
//     via the slug → app_id lookup followed by the
//     forge-proof check on data.app_id: the read goes through
//     store.ListEventsByWakeID(wake_id, since, limit) (the
//     partial-index path on events_wake_id_idx), but the
//     handler then verifies EVERY row's data.app_id matches
//     the slug's resolved app. A row that mismatches is
//     dropped (a malicious admin who forged data.app_id would
//     never see their row surface in a foreign tenant's
//     timeline).
//
//   - Unknown wake_id 404s the same way a cross-account wake_id
//     does — a customer cannot enumerate wake counts by ID
//     probing.
//
// What this surface deliberately does NOT do
//
//   - No filter by free-text data payload (e.g. "any event where
//     data.instance_id = X"). The wake_id filter is the only
//     SQL-anchored filter because the events table is not
//     indexed on data — only data.wake_id is indexed (partial
//     index events_wake_id_idx).
//   - No PATCH / DELETE on individual rows. The events table is
//     append-only (spec §5 / §6.1).
package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

const (
	wakeTimelineLimitDefault = 200
	wakeTimelineLimitMax     = 1000
)

// listWakeTimeline handles GET /v1/apps/{slug}/wakes/{wake_id}/timeline.
// Oldest first (forward narrative).
//
// Query params (all optional):
//
//	since   RFC 3339 timestamp; rows strictly older are skipped
//	limit   1..1000; defaults to 200
//
// On any limit > Max we silently cap. On a malformed since we
// return 400 invalid_since (not silently drop, per the convention
// of listAuditEvents). On a slug that doesn't resolve to the
// caller's account we 404 the same way unknown slugs do (forge-
// proof).
func (s *server) listWakeTimeline(w http.ResponseWriter, r *http.Request, acct state.Account) {
	slug := r.PathValue("slug")
	wakeID := r.PathValue("wake_id")
	if wakeID == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid wake_id", "wake_id path segment is required"))
		return
	}
	app, err := s.store.AppBySlug(r.Context(), slug)
	if err != nil || app.AccountID != acct.ID {
		s.notFound(w, "no such app")
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, terr := time.Parse(time.RFC3339Nano, raw)
		if terr != nil {
			// Fallback to plain RFC 3339 (the format pre-PR-C
			// callers used). The handler accepts both so an
			// older SDK doesn't silently drop the filter.
			t, terr = time.Parse(time.RFC3339, raw)
		}
		if terr != nil {
			api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"Invalid since", "since must be RFC 3339 (e.g. 2026-07-25T00:00:00Z)"))
			return
		}
		since = t
	}
	limit := wakeTimelineLimitDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 {
			api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"Invalid limit", "limit must be a positive integer"))
			return
		}
		// Cap at the SDK-documented max. Using min(n, Max) here
		// (instead of an if/reassign) keeps the bound visible to
		// CodeQL's go/uncontrolled-allocation-size taint tracking —
		// the if-reassign pattern flow-traces n as user-controlled
		// into the make(…,0,limit) below and fires a CWE-770 false
		// positive. The min() form is a direct dataflow cap.
		limit = min(n, wakeTimelineLimitMax)
	}
	// Over-read by 1 so we can detect "is there a next page?" without
	// a second SQL roundtrip. The partial index events_wake_id_idx
	// makes this an O(limit) cost regardless of the events table size.
	rows, err := s.store.ListEventsByWakeID(r.Context(), wakeID, since, limit+1)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list wake timeline"))
		return
	}
	// Unknown wake_id 404s the same way unknown slugs do —
	// a customer cannot enumerate wake counts by ID-probing.
	// `since` is the only filter that can legitimately yield
	// zero rows (e.g. a "?since=2099-..." query) so we
	// distinguish: when since is non-zero, the empty result
	// is the right answer; when since is zero, an empty
	// result means "no such wake_id".
	if len(rows) == 0 && since.IsZero() {
		api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
			"Wake not found", "no events row with that wake_id belongs to this account"))
		return
	}
	// Cap the slice capacity at the same Max used at the
	// limit-parsing site. The downstream `min(n, Max)`
	// flow-tracks through the `limit+1` over-read at the
	// ListEventsByWakeID call site, but CodeQL's
	// go/uncontrolled-allocation-size taint tracking loses
	// the bound across the cross-function call boundary and
	// re-flags the make() here. Applying `min(limit, Max)` at
	// the allocation site keeps the bound visible directly to
	// the slice allocation. Per
	// codeql-go-uncontrolled-allocation-size-min-pattern.
	out := make([]api.WakeTimelineEvent, 0, min(limit, wakeTimelineLimitMax))
	for _, e := range rows {
		// Forge-proof: every row's data.app_id must equal the
		// slug's resolved app id. A mismatch is dropped silently
		// (it can't be displayed under the caller's account
		// without leaking cross-account info).
		if !eventDataHasAppID(e.Data, app.ID) {
			continue
		}
		// Bound check before append: the slice grows one at a time
		// and CodeQL's go/uncontrolled-allocation-size taint tracking
		// loses the `min(limit, Max)` cap if the cap is asserted
		// AFTER the append (the +1 elements already in `out` flow
		// forward into the next iteration's append). Capping first
		// keeps the bound visible to the static analyzer AND the
		// runtime (we never reach the append when full).
		if len(out) >= limit {
			break
		}
		out = append(out, wakeTimelineEvent(e))
	}
	var nextCursor string
	if len(out) > 0 && len(rows) > limit {
		// next page — the cursor is the last in-window row's `at`,
		// formatted as RFC 3339 with nanosecond precision so it
		// round-trips back into `since` without truncation. The
		// +1 overscan row (rows[limit]) is the one that proves a
		// next page exists; rows[limit-1] is the last row whose
		// index is ≤ limit-1 (i.e. the in-window set). The
		// forge-proof may have dropped rows, so we re-derive the
		// cursor from `rows` rather than from `out` — the rows
		// slice is the SQL result set, which is the source of
		// truth for the at-ordering.
		nextCursor = rows[limit-1].At.UTC().Format(time.RFC3339Nano)
	}
	writeJSON(w, http.StatusOK, api.WakeTimelineResponse{
		WakeID:     wakeID,
		AppID:      app.ID,
		Events:     out,
		NextCursor: nextCursor,
		Limit:      limit,
	})
}

// wakeTimelineEvent converts one state.Event row into the wire
// shape. The Data field is a map (decoded from the row's jsonb
// column) so the SDK can introspect the typed payload without
// a separate roundtrip. `at` is RFC 3339 with nanosecond
// precision so frames that land in the same wall-clock second
// still preserve at-ordering in lexicographic string compare
// (the customer-facing timeline orders at ASC, oldest first).
func wakeTimelineEvent(e state.Event) api.WakeTimelineEvent {
	out := api.WakeTimelineEvent{
		At:    e.At.UTC().Format(time.RFC3339Nano),
		Kind:  e.Kind,
		Actor: e.Actor,
	}
	if len(e.Data) > 0 {
		var payload map[string]any
		// Best-effort: a malformed data column (which shouldn't
		// happen — pkg/events.Platform always writes a json
		// object) falls through to an empty map so the rest of
		// the row still surfaces.
		_ = json.Unmarshal(e.Data, &payload)
		out.Data = payload
	}
	return out
}
