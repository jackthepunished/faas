// handlers_audit.go — IAM-4 (ADR-035) auth audit event surface.
//
// Routes (registered in server.go::handler):
//
//	GET /v1/audit-events            → listAuditEvents
//	GET /v1/audit-events/{id}       → getAuditEvent
//
// Trust model
//
//   - Both routes sit behind s.auth + requireScope(http.MethodGet,
//     api.ScopeAdmin, api.MethodDefaultScope(http.MethodGet)) — the
//     same gating as GET /v1/keys. A session-cookie principal (Key ==
//     nil) implicitly carries admin scope per principalHasScope, so a
//     dashboard customer can read their own log without holding an
//     API key. A read-scoped or write-scoped API key works too.
//
//   - Cross-account invisibility is enforced at the SQL layer via
//     store.ListEvents(acct.ID, ...) — the events_subject_idx
//     composite index (migrations/00002_app_manifest_and_domains.sql)
//     already filters by (subject, at desc), so even an over-read by
//     200 rows for the prefix filter costs the planner only an index
//     scan.
//
//   - getAuditEvent re-uses the same subject-pinned ListEvents result
//     so a customer cannot enumerate other accounts' events by guessing
//     bigints. A cross-account id 404s the same way an unknown id does.
//
// What this surface deliberately does NOT do
//
//   - No pagination beyond a fixed limit (default 50, max 100). The
//     GDPR export bundle is the canonical artifact for full-history
//     reads; this endpoint is the daily-driver "who deleted my key
//     last Tuesday?" UI surface.
//   - No filter by free-text data payload (e.g. "any event where
//     data.app_id = X"). kind_prefix is the only filter because the
//     events table is not indexed on data — an opportunistic scan
//     would force a sequential read.
//   - No PATCH / DELETE on individual rows. The events table is
//     append-only (spec §5 / §6.1); the spec doesn't grant customers
//     a tamper interface and the auditor helper never exposes one.

package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

// listAuditEventsLimitMin / Max bound the ?limit query param. The
// minimum is implicitly 1 (zero means "default"); values <1 fall back
// to listAuditEventsLimitDefault.
const (
	listAuditEventsLimitDefault = 50
	listAuditEventsLimitMax     = 100
	// listAuditEventsOverRead is the hard cap passed to ListEvents when
	// the since / kind_prefix filters are in play. Picking 200 matches
	// the spec cap (most customer accounts emit <<10 audit rows/day) and
	// keeps the per-request DB cost bounded.
	listAuditEventsOverRead = 200
)

// listAuditEvents handles GET /v1/audit-events. Newest first.
//
// Query params (all optional):
//
//	since        RFC 3339 timestamp; rows strictly older are skipped
//	kind_prefix  e.g. "key." returns only "key.created" / "key.deleted"
//	limit        1..100; defaults to 50
//
// On any limit > Max we silently cap (per the spec convention used by
// the rest of apid's list handlers — see GET /v1/crons). On a malformed
// since we return 400 invalid_since rather than silently dropping the
// filter, because silently ignoring the time floor would let a buggy
// SDK pin a customer to "everything since forever".
func (s *server) listAuditEvents(w http.ResponseWriter, r *http.Request, acct state.Account) {
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"Invalid since", "since must be RFC 3339 (e.g. 2026-07-25T00:00:00Z)"))
			return
		}
		since = t
	}
	prefix := r.URL.Query().Get("kind_prefix")
	limit := listAuditEventsLimitDefault
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
				"Invalid limit", "limit must be a positive integer"))
			return
		}
		if n > listAuditEventsLimitMax {
			n = listAuditEventsLimitMax
		}
		limit = n
	}

	// Over-read to honor the since + kind_prefix filters at the
	// application layer. The composite index (subject, at desc) makes
	// the SQL query return the 200 newest rows for the account in
	// O(200) regardless of the table size; the in-Go filter walks
	// that window and stops as soon as the limit is filled.
	rows, err := s.store.ListEvents(r.Context(), acct.ID, listAuditEventsOverRead)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list audit events"))
		return
	}
	out := make([]api.AuditEventResponse, 0, limit)
	for _, e := range rows {
		if !since.IsZero() && e.At.Before(since) {
			continue
		}
		if prefix != "" && !strings.HasPrefix(e.Kind, prefix) {
			continue
		}
		out = append(out, auditEventResponse(e))
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, api.ListAuditEventsResponse{
		Events: out,
		Limit:  limit,
	})
}

// getAuditEvent handles GET /v1/audit-events/{id}. The id is the bigint
// primary key of the events row. Cross-account lookups 404 the same
// way an unknown id does, so a customer cannot enumerate other
// accounts' row counts by ID-probing.
//
// The mux route is "GET /v1/audit-events/{id}", so r.PathValue("id")
// is always non-empty here — the empty-string branch below is a
// defensive check that should never fire in production; it stays as
// belt-and-braces in case a future mount re-registers the path
// without a {id} segment.
func (s *server) getAuditEvent(w http.ResponseWriter, r *http.Request, acct state.Account) {
	id := r.PathValue("id")
	if id == "" {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid id", "id path segment is required"))
		return
	}
	// Parse once to make sure it's actually a bigint-shaped string —
	// bad input should 400, not 404. Bigints > MaxInt64 are unreachable
	// in practice (the events table is append-only since pre-launch).
	target, err := strconv.ParseInt(id, 10, 64)
	if err != nil || target <= 0 {
		api.WriteProblem(w, api.NewProblem(http.StatusBadRequest, api.CodeValidation,
			"Invalid id", "id must be a positive integer"))
		return
	}
	rows, err := s.store.ListEvents(r.Context(), acct.ID, listAuditEventsOverRead)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not list audit events"))
		return
	}
	for _, e := range rows {
		if e.ID == target {
			writeJSON(w, http.StatusOK, auditEventResponse(e))
			return
		}
	}
	api.WriteProblem(w, api.NewProblem(http.StatusNotFound, api.CodeNotFound,
		"Audit event not found", "no event with that id belongs to this account"))
}

// auditEventResponse converts one state.Event row into the wire shape.
// Subject is rendered as a string (uuid canonical form) — the wire
// contract is string-typed so JSON consumers never see Go's uuid type.
func auditEventResponse(e state.Event) api.AuditEventResponse {
	resp := api.AuditEventResponse{
		ID:    strconv.FormatInt(e.ID, 10),
		At:    e.At.UTC().Format(time.RFC3339),
		Actor: e.Actor,
		Kind:  e.Kind,
		Data:  e.Data,
	}
	if e.Subject != nil {
		resp.Subject = e.Subject.String()
	}
	return resp
}
