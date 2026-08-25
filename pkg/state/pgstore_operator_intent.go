package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// pgstore_operator_intent.go — CRUD for the operator_intents
// table (migrations/00438, PR #1099 P2 redesign).
//
// Two producers, one consumer:
//
//   - apid (cmd/apid/handlers_admin_force_park.go and the
//     cold-boot twin) is the only caller of
//     InsertOperatorIntent. Status starts at `pending`. After the
//     INSERT, apid emits `db.NotifyOperatorIntent` so schedd
//     picks up the row on the next LISTEN delivery.
//   - schedd (pkg/sched/operator_intent_subscriber.go) is the
//     only caller of ClaimPendingOperatorIntent /
//     MarkOperatorIntentSucceeded / MarkOperatorIntentFailed.
//     It updates status to `running` on claim and to a
//     terminal value after Engine.Park /
//     Engine.ForceColdBootNextWake returns.
//
// Cross-account safety: every helper takes an explicit
// target_id + account_id parameter; schedd never queries by
// account_id alone (the claim query selects by
// status='pending' ORDER BY requested_at — the row's
// account_id is for audit-log correlation, not access
// control). The API surface is gated by the IDOR check in
// forcePark / forceColdBoot via the existing apid allowlist
// (admin scope + s.adminAllows).

// ErrOperatorIntentNotFound is returned by Mark* / Get helpers
// when no row matches the given intent id. Distinct from
// ErrNotFound so callers can tell "the row vanished" from
// upstream errors.
var ErrOperatorIntentNotFound = errors.New("state: operator intent not found")

// InsertOperatorIntent writes a new pending row and returns the
// generated id. Caller (apid) is responsible for the
// subsequent `pg_notify('operator_intent', payload)`; the helper
// does NOT emit the notify so the producer has a clean
// transactional boundary (the row is committed before the
// notify fires — same precedent as InsertFireNowRequest).
//
// account_id may be nil for fleet-level actions (none today;
// reclaim_build — P2c — stays on a separate code path); the
// column is nullable in the schema for forward-compatibility.
// actor_id is the admin actor identity (caller); metadata is
// free-form JSONB for future per-kind payload fields (snap IDs
// at claim time, etc.) and defaults to '{}'.
//
// Both ids are passed as strings for symmetry with the rest
// of the pgstore surface (uuid.UUID casts inline below).
func (s *PgStore) InsertOperatorIntent(
	ctx context.Context,
	kind OperatorIntentKind,
	targetID string,
	accountID *string,
	actorID string,
	reason string,
	metadata json.RawMessage,
) (string, error) {
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}
	id := uuid.NewString()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO operator_intents
		    (id, kind, target_id, account_id, actor_id, reason, metadata, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')
	`, id, string(kind), targetID, accountID, actorID, reason, metadata)
	if err != nil {
		return "", fmt.Errorf("state: insert operator_intent: %w", err)
	}
	return id, nil
}

// ClaimPendingOperatorIntent atomically transitions one
// pending row to `running` and returns it. The SKIP LOCKED
// clause lets N schedd instances race for the wakeup without
// blocking each other; an already-claimed row is invisible
// to the next caller.
//
// Returns ErrOperatorIntentNotFound when no pending row exists
// (caller stops processing this tick). On success the returned
// row's Status is `running` and the caller is expected to
// update it to a terminal value via
// MarkOperatorIntentSucceeded / MarkOperatorIntentFailed.
//
// The TX is a single-statement transaction so the claim +
// status update are atomic against the rest of the system.
// The 5 s statement timeout matches pgstore.go defaults.
func (s *PgStore) ClaimPendingOperatorIntent(ctx context.Context) (OperatorIntent, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return OperatorIntent{}, fmt.Errorf("state: claim operator_intent begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id, kindStr, targetID, actorID, reason, metadataStr string
		accountID, startedAt, finishedAt, errMsg            *string
		snapIDsMarkedStale                                  []string
		requestedAt                                         time.Time
	)
	row := tx.QueryRow(ctx, `
		SELECT id, kind, target_id, account_id, actor_id, reason,
		       metadata::text, status, requested_at, started_at,
		       finished_at, COALESCE(error, ''),
		       snap_ids_marked_stale
		FROM operator_intents
		WHERE status = 'pending'
		ORDER BY requested_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`)
	if err := row.Scan(&id, &kindStr, &targetID, &accountID, &actorID, &reason,
		&metadataStr, new(string), &requestedAt, &startedAt, &finishedAt,
		&errMsg, &snapIDsMarkedStale); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OperatorIntent{}, ErrOperatorIntentNotFound
		}
		return OperatorIntent{}, fmt.Errorf("state: claim operator_intent scan: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE operator_intents
		SET status = 'running',
		    started_at = now()
		WHERE id = $1
	`, id); err != nil {
		return OperatorIntent{}, fmt.Errorf("state: claim operator_intent update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return OperatorIntent{}, fmt.Errorf("state: claim operator_intent commit: %w", err)
	}
	return decodeOperatorIntentRow(id, kindStr, targetID, actorID, reason,
		accountID, startedAt, finishedAt, errMsg,
		metadataStr, OperatorIntentRunning, requestedAt,
		snapIDsMarkedStale), nil
}

// MarkOperatorIntentSucceeded stamps the row's terminal state.
// snapIDs is the list of stale snapshot IDs (force_cold_boot
// only; force_park passes nil). finished_at is server-stamped
// to wall-clock now; callers do not pass it.
func (s *PgStore) MarkOperatorIntentSucceeded(ctx context.Context, id string, snapIDs []string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE operator_intents
		SET status = 'succeeded',
		    finished_at = $2,
		    snap_ids_marked_stale = $3
		WHERE id = $1 AND status = 'running'
	`, id, time.Now().UTC(), snapIDs)
	if err != nil {
		return fmt.Errorf("state: mark operator_intent succeeded: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOperatorIntentNotFound
	}
	return nil
}

// MarkOperatorIntentFailed stamps the row's terminal state
// with the failure message. Used for both dispatch failures
// (Engine.Park returns ErrCanTransition because the row is
// already PARKED) and for errors that surface mid-dispatch
// (e.g. deployment not found).
//
// errMsg is the free-text error string the operator will see
// via `GET /v1/admin/operator-intents/{id}`. Cap to 1 KB to
// match the audit payload convention
// (pkg/sched/loop.go:1840-1864).
func (s *PgStore) MarkOperatorIntentFailed(ctx context.Context, id, errMsg string) error {
	if len(errMsg) > 1024 {
		errMsg = errMsg[:1024]
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE operator_intents
		SET status = 'failed',
		    error = $2,
		    finished_at = $3
		WHERE id = $1 AND status = 'running'
	`, id, errMsg, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("state: mark operator_intent failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOperatorIntentNotFound
	}
	return nil
}

// GetOperatorIntent reads one row by id. Used by the API
// surface to poll request status
// (GET /v1/admin/operator-intents/{id}).
func (s *PgStore) GetOperatorIntent(ctx context.Context, id string) (OperatorIntent, error) {
	var (
		kindStr, targetID, actorID, reason, metadataStr string
		accountID, startedAt, finishedAt, errMsg        *string
		snapIDsMarkedStale                              []string
		requestedAt                                     time.Time
		statusStr                                       string
	)
	row := s.pool.QueryRow(ctx, `
		SELECT id, kind, target_id, account_id, actor_id, reason,
		       metadata::text, status, requested_at, started_at,
		       finished_at, COALESCE(error, ''),
		       snap_ids_marked_stale
		FROM operator_intents
		WHERE id = $1
	`, id)
	if err := row.Scan(&id, &kindStr, &targetID, &accountID, &actorID, &reason,
		&metadataStr, &statusStr, &requestedAt, &startedAt, &finishedAt,
		&errMsg, &snapIDsMarkedStale); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OperatorIntent{}, ErrOperatorIntentNotFound
		}
		return OperatorIntent{}, fmt.Errorf("state: get operator_intent: %w", err)
	}
	return decodeOperatorIntentRow(id, kindStr, targetID, actorID, reason,
		accountID, startedAt, finishedAt, errMsg,
		metadataStr, OperatorIntentStatus(statusStr), requestedAt,
		snapIDsMarkedStale), nil
}

// ReclaimStuckRunningOperatorIntents resets `running` rows
// older than the threshold back to `pending` so the next
// ClaimPendingOperatorIntent picks them up. The UPDATE clears
// started_at because the row is no longer "in-flight" —
// the next claim will stamp a fresh started_at. The SELECT
// is intentionally absent: a single statement-update is
// atomic and avoids the TOCTOU window a SELECT-then-UPDATE
// pair would open (a row could be MarkSucceeded between
// the two statements and we'd overwrite the terminal state).
//
// Returns the row count via RowsAffected; the caller logs at
// INFO when the count is non-zero (a steady-state value of 0
// is expected — the safety tick fires every 30s and only
// reclaiming rows after the 5min timeout keeps this number
// small).
func (s *PgStore) ReclaimStuckRunningOperatorIntents(ctx context.Context, threshold time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE operator_intents
		SET status = 'pending',
		    started_at = NULL
		WHERE status = 'running'
		  AND started_at IS NOT NULL
		  AND started_at < $1
	`, threshold)
	if err != nil {
		return 0, fmt.Errorf("state: reclaim stuck running operator_intent: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// decodeOperatorIntentRow is the shared row → struct
// converter used by Claim / Get. Scans return string columns
// from the SQL `text` casts (kind, status) and pointer columns
// for nullable fields (account_id, started_at, finished_at,
// error). The decoder wraps the raw strings into the typed
// OperatorIntent / OperatorIntentStatus values.
func decodeOperatorIntentRow(
	id, kindStr, targetID, actorID, reason string,
	accountID, startedAt, finishedAt, errMsg *string,
	metadataStr string,
	status OperatorIntentStatus,
	requestedAt time.Time,
	snapIDsMarkedStale []string,
) OperatorIntent {
	// Fill nullable fields with empty sentinels — Get claims
	// these as nil rather than '', so a nil → "" coercion
	// avoids surprising callers that do `if i.AccountID == ""`.
	var acct, started, fin, e string
	if accountID != nil {
		acct = *accountID
	}
	if startedAt != nil {
		started = *startedAt
	}
	if finishedAt != nil {
		fin = *finishedAt
	}
	if errMsg != nil {
		e = *errMsg
	}
	var startedT, finishedT *time.Time
	if started != "" {
		if pt, err := time.Parse(time.RFC3339Nano, started); err == nil {
			startedT = &pt
		}
	}
	if fin != "" {
		if pt, err := time.Parse(time.RFC3339Nano, fin); err == nil {
			finishedT = &pt
		}
	}
	return OperatorIntent{
		ID:                 id,
		Kind:               OperatorIntentKind(kindStr),
		TargetID:           targetID,
		AccountID:          ptrIfNonEmpty(acct),
		ActorID:            actorID,
		Reason:             reason,
		Metadata:           json.RawMessage(metadataStr),
		Status:             status,
		RequestedAt:        requestedAt,
		StartedAt:          startedT,
		FinishedAt:         finishedT,
		Error:              e,
		SnapIDsMarkedStale: snapIDsMarkedStale,
	}
}

func ptrIfNonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
