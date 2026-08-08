// Billing-related API DTOs that live in pkg/api so the JSON wire
// shape is decoupled from the provider-specific package.
//
// PR-P3: BillingReconcileResponse is the response shape for POST
// /v1/admin/billing-reconcile/{id}. The handler is in
// cmd/apid/handlers_admin_billing.go and reads usage_minutes via
// billing.Provider.ReconcileUsage; the response surfaces the
// total mb_seconds for the [start, end) window so operators can
// diff the SDK-side number against the local usage_minutes sum.
//
// Money: integer mb_seconds. No floats anywhere on this struct —
// the underlying values are integer counters summed across
// usage_minutes rows.
package api

import "time"

// BillingReconcileResponse is the JSON shape POST
// /v1/admin/billing-reconcile/{id} returns on success (200).
//
// AccountID echoes the path value so the CLI's loop output is
// self-describing without parsing the URL. Start / End are RFC 3339
// timestamps in UTC, formatted as 2006-01-02T15:04:05Z07:00 by the
// handler. MBSeconds is the integer total the provider SDK returned
// for the window; for Stripe this is the usage-record sum, for
// Paddle the handler 501s (provider returns ErrNotImplemented).
type BillingReconcileResponse struct {
	AccountID string    `json:"account_id"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	MBSeconds int64     `json:"mb_seconds"`
}

// BillingCatalogKind discriminates the entries inside BillingCatalogResponse.
//
// Paddle exposes two distinct price types per plan (the recurring
// monthly subscription and the flat-rate overage line item) plus a
// parent product handle. Mirrors paddle.CatalogKind in the response
// wire — duplicated here so pkg/api can stay leaf (no paddle import).
type BillingCatalogKind string

const (
	BillingCatalogKindMonthly BillingCatalogKind = "monthly"
	BillingCatalogKindOverage BillingCatalogKind = "overage"
	BillingCatalogKindProduct BillingCatalogKind = "product"
)

// BillingCatalogEntry is one row in the price + product catalog.
// Plan values are the billable api.Plan constants ("hobby", "pro",
// "scale") — PlanFree is intentionally absent because it carries
// no recurring line item. Handle is the Paddle-side id (pri_… for
// monthly / overage, pro_… for product). SyncedAt is RFC 3339 UTC
// from the catalog's lastSyncAt; the zero-value renders as
// "0001-01-01T00:00:00Z" via the standard JSON marshaler.
type BillingCatalogEntry struct {
	Plan     string             `json:"plan"`
	Kind     BillingCatalogKind `json:"kind"`
	Handle   string             `json:"handle"`
	SyncedAt time.Time          `json:"synced_at"`
}

// BillingCatalogResponse is the wire shape for the
// GET/POST/DELETE /v1/admin/billing-paddle-catalog endpoints.
// Provider is the active billing provider's name (paddle / stripe);
// on a Stripe deployment the handler 501s before serializing this
// struct, so the field only ever carries a real value on the
// Paddle path. SyncedAt is the timestamp of the most recent
// successful EnsurePlanProducts call; empty string when no
// hydration has yet completed (POST and DELETE both reset it).
type BillingCatalogResponse struct {
	Provider string                `json:"provider"`
	SyncedAt string                `json:"synced_at"`
	Entries  []BillingCatalogEntry `json:"entries"`
}
