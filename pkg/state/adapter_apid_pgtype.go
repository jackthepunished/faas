// Public pgtype adapters for cmd/apid.
//
// cmd/apid must not import pgtype directly (the depguard
// apid-control-plane rule excludes pgx/pgtype imports from
// cmd/apid/* paths that don't own the connection — see
// .golangci.yml lines 50-78). The gRPC server-side handler
// (cmd/apid/grpc_server_apperrors.go) takes sqlc.IncrementAppErrorParams
// + sqlc.InsertAppErrorRequestParams, both of which embed
// pgtype.UUID / pgtype.Timestamptz. This file exposes the
// minimal google/uuid.UUID ↔ pgtype.UUID and time.Time ↔
// pgtype.Timestamptz bridges so cmd/apid doesn't have to reach
// into the unexported pkg/state/pgstore.go helpers.
//
// These types are aliases of pgtype.UUID / pgtype.Timestamptz,
// so cmd/apid can pass them directly into sqlc params without
// an extra conversion. The pgtype.UUID / pgtype.Timestamptz
// types are themselves just structs with a Valid flag, so the
// aliasing is a no-op at runtime.
//
// Keep these in sync with pkg/state/pgstore.go's private
// uuidFromPgtype / timeFromPgtype / pgtypeFromUUID /
// pgtypeFromUUIDPtr / pgtypeFromTime. Any drift here is a build
// break (the cmd/apid side compiles but stores corrupt values).

package state

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// UUID is the sqlc/pgtype.UUID exposed under the state
// package's surface so cmd/apid can build sqlc params without
// importing pgtype directly.
type UUID = pgtype.UUID

// Timestamptz is the sqlc/pgtype.Timestamptz exposed under the
// state package's surface so cmd/apid can build sqlc params
// without importing pgtype directly.
type Timestamptz = pgtype.Timestamptz

// NewPgtypeUUID wraps google/uuid.UUID into pgtype.UUID with
// Valid=true. Use this for NOT NULL uuid columns (account_id,
// app_id, fingerprint lookup keys).
func NewPgtypeUUID(u uuid.UUID) UUID {
	return pgtype.UUID{Bytes: u, Valid: true}
}

// NewPgtypeUUIDPtr wraps *uuid.UUID into pgtype.UUID (NULL
// when the pointer is nil). Use this for nullable uuid
// columns (deployment_id).
func NewPgtypeUUIDPtr(u *uuid.UUID) UUID {
	if u == nil {
		return UUID{}
	}
	return NewPgtypeUUID(*u)
}

// NewPgtypeTime wraps time.Time into pgtype.Timestamptz. A
// zero time → NULL; anything else → UTC-normalised and Valid.
func NewPgtypeTime(t time.Time) Timestamptz {
	if t.IsZero() {
		return Timestamptz{}
	}
	return Timestamptz{Time: t.UTC(), Valid: true}
}
