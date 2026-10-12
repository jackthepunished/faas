package apidserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/state"
)

type deletedAppCursor struct {
	AccountID string    `json:"account_id"`
	At        time.Time `json:"at"`
	ID        string    `json:"id"`
}

func validDeletedAppID(id string) bool {
	// MemStore's historical IDs are 32 hex characters; PostgreSQL returns UUIDs.
	if len(id) != 32 && len(id) != 36 {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

func decodeDeletedAppCursor(token, accountID string) (deletedAppCursor, error) {
	var cursor deletedAppCursor
	if token == "" {
		return cursor, nil
	}
	if len(token) > 1024 {
		return cursor, errors.New("cursor exceeds its bound")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return cursor, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return cursor, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return cursor, errors.New("cursor must contain a single value")
	}
	if cursor.AccountID != accountID || cursor.At.IsZero() || !validDeletedAppID(cursor.ID) {
		return cursor, errors.New("cursor is not valid for this account")
	}
	return cursor, nil
}

func encodeDeletedAppCursor(accountID string, row state.DeletedAppRecord) string {
	raw, _ := json.Marshal(deletedAppCursor{AccountID: accountID, At: row.SortAt, ID: row.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func deletedAppSummary(row state.DeletedAppRecord, accountID string, now time.Time) api.DeletedAppSummary {
	out := api.DeletedAppSummary{
		AccountID: accountID, ID: row.ID, Slug: row.Slug, DeletedAt: row.DeletedAt,
		DeleteGraceUntil: row.DeleteGraceUntil, EvaluatedAt: now,
	}
	switch {
	case row.PurgeStarted:
		out.RestoreBlocker = "purge_started"
	case row.DeletedAt == nil || row.DeleteGraceUntil == nil:
		out.RestoreBlocker = "metadata_unavailable"
	case !row.DeleteGraceUntil.After(now):
		out.RestoreBlocker = "grace_expired"
	default:
		out.RestoreEligible = true
	}
	return out
}

func (s *server) deletedAppsStore(w http.ResponseWriter) (state.AccountDeletedAppsStore, bool) {
	store, ok := s.store.(state.AccountDeletedAppsStore)
	if !ok {
		api.WriteProblem(w, api.ErrCapacity("account-owned deleted-app inventory is unavailable"))
	}
	return store, ok
}

func validDeletedAppQuery(r *http.Request, pagination bool) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	for key, values := range query {
		if !pagination || (key != "limit" && key != "cursor") || len(values) != 1 {
			return false
		}
	}
	return true
}

func (s *server) listDeletedApps(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !validDeletedAppQuery(r, true) {
		api.WriteProblem(w, api.ErrValidation("only one limit and cursor are supported; environment scope is not supported"))
		return
	}
	limit := 25
	if value, present := r.URL.Query()["limit"]; present {
		parsed, err := strconv.Atoi(value[0])
		if err != nil || parsed < 1 || parsed > 50 {
			api.WriteProblem(w, api.ErrValidation("limit must be 1..50"))
			return
		}
		limit = parsed
	}
	cursor, err := decodeDeletedAppCursor(r.URL.Query().Get("cursor"), acct.ID)
	if err != nil {
		api.WriteProblem(w, api.ErrValidation("invalid deleted-app cursor; restart this account's inventory"))
		return
	}
	store, ok := s.deletedAppsStore(w)
	if !ok {
		return
	}
	rows, err := store.ListAccountDeletedApps(r.Context(), acct.ID, cursor.At, cursor.ID, limit+1)
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not read deleted-app inventory"))
		return
	}
	now := time.Now().UTC()
	out := api.DeletedAppListResponse{AccountID: acct.ID, Apps: make([]api.DeletedAppSummary, 0, limit), EvaluatedAt: now}
	if len(rows) > limit {
		out.NextCursor = encodeDeletedAppCursor(acct.ID, rows[limit-1])
		rows = rows[:limit]
	}
	for _, row := range rows {
		out.Apps = append(out.Apps, deletedAppSummary(row, acct.ID, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) readDeletedApp(w http.ResponseWriter, r *http.Request, acct state.Account) (state.DeletedAppRecord, bool) {
	if !validDeletedAppID(r.PathValue("id")) {
		api.WriteProblem(w, api.ErrValidation("invalid deleted-app ID"))
		return state.DeletedAppRecord{}, false
	}
	store, ok := s.deletedAppsStore(w)
	if !ok {
		return state.DeletedAppRecord{}, false
	}
	row, err := store.AccountDeletedAppByID(r.Context(), acct.ID, r.PathValue("id"))
	if errors.Is(err, state.ErrNotFound) {
		s.notFound(w, "no such deleted app")
		return state.DeletedAppRecord{}, false
	}
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not read deleted app"))
		return state.DeletedAppRecord{}, false
	}
	return row, true
}

func (s *server) getDeletedApp(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !validDeletedAppQuery(r, false) {
		api.WriteProblem(w, api.ErrValidation("deleted-app detail does not accept environment or other query scope"))
		return
	}
	row, ok := s.readDeletedApp(w, r, acct)
	if ok {
		writeJSON(w, http.StatusOK, deletedAppSummary(row, acct.ID, time.Now().UTC()))
	}
}

func (s *server) restoreDeletedApp(w http.ResponseWriter, r *http.Request, acct state.Account) {
	if !validDeletedAppQuery(r, false) {
		api.WriteProblem(w, api.ErrValidation("restore does not accept environment or other query scope"))
		return
	}
	// Only an absent body or an empty object represents this one action. Never
	// interpret an unknown intent (for example dry_run) as a real restoration.
	var body *struct{}
	err := decodeJSONSized(r, &body, 4096)
	if err != io.EOF && (err != nil || body == nil) {
		api.WriteProblem(w, api.ErrValidation("restore accepts no fields; use an empty body"))
		return
	}
	row, ok := s.readDeletedApp(w, r, acct)
	if !ok {
		return
	}
	lifecycle := deletedAppSummary(row, acct.ID, time.Now().UTC())
	if !lifecycle.RestoreEligible {
		api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeAppNotRestorable,
			"App not restorable", "the lifecycle review is blocked: "+lifecycle.RestoreBlocker))
		return
	}
	app, err := s.store.AppByID(r.Context(), row.ID)
	if errors.Is(err, state.ErrNotFound) || (err == nil && app.AccountID != acct.ID) {
		s.notFound(w, "no such deleted app")
		return
	}
	if err != nil {
		api.WriteProblem(w, api.ErrCapacity("could not recheck deleted app"))
		return
	}
	if app.ID != row.ID || app.Slug != row.Slug || app.Status != state.AppDeleted {
		writeRestoreIdentityConflict(w)
		return
	}
	current, err := s.store.AppBySlug(r.Context(), row.Slug)
	if err == nil && current.ID != row.ID {
		api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeConflict,
			"App slug is in use", "the retained app's slug belongs to an active app; inspect the inventory before a new restore review"))
		return
	}
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		api.WriteProblem(w, api.ErrCapacity("could not recheck app slug"))
		return
	}
	// RestoreApp/RestoreAppWithActivity conditionally mutate this exact ID and
	// recheck grace, purge claim, quota and capacity at the mutation boundary.
	s.restoreOwnedApp(w, r, acct, app)
}

func writeRestoreIdentityConflict(w http.ResponseWriter) {
	api.WriteProblem(w, api.NewProblem(http.StatusConflict, api.CodeConflict,
		"App identity changed", "the reviewed deleted app changed; refresh its exact ID and review again"))
}

func readLegacyRestoreIdentity(r *http.Request) (string, error) {
	var request *struct {
		ExpectedAppID json.RawMessage `json:"expected_app_id"`
	}
	if err := decodeJSONSized(r, &request, 4096); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", err
	}
	if request == nil {
		return "", errors.New("restore body must be an object")
	}
	if request.ExpectedAppID == nil {
		return "", nil
	}
	var id string
	if err := json.Unmarshal(request.ExpectedAppID, &id); err != nil || !validDeletedAppID(id) {
		return "", errors.New("expected_app_id must be a nonempty app ID")
	}
	return id, nil
}
