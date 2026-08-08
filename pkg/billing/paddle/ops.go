// PR-P3: Operator-side inspect / sync / reset surface for the Paddle
// price catalog. The Paddle catalog is the durable source of truth
// (api.sandbox.paddle.com for sandbox, api.paddle.com for production);
// the *Provider caches the resolved price handles in-memory so the
// meterd pusher loop + the changePlan path can read them without a
// per-call SDK round trip.
//
// This file defines:
//
//   - CatalogEntry — the wire shape returned to operators via the
//     admin HTTP endpoint + the CLI.
//   - OpProvider — the interface the admin handler type-asserts against
//     (providerOpsFor). Stripe doesn't implement it, so the type
//     assertion fails there and the handler returns 501 — a deliberate
//     "this provider does not support that operation" surface rather
//     than a panic.
//
// The implementation lives on *Provider (methods below). The interface
// is intentionally narrow: read the catalog, force a sync, signal a
// reset. The CLI / handler surface is a thin pass-through; all the
// domain logic stays on *Provider.
package paddle

import (
	"context"
	"time"

	"github.com/onebox-faas/faas/pkg/api"
)

// CatalogKind discriminates the entries in CatalogEntry. Paddle
// exposes two distinct price types per plan — the recurring monthly
// subscription and the flat-rate overage line item — so an operator
// inspecting the catalog needs both. The "product" kind carries the
// parent product ID (pro_…) which is what `CreateUpgradeTransaction`
// reads to derive the customer-binding product context.
type CatalogKind string

const (
	CatalogKindMonthly CatalogKind = "monthly" // pri_… subscription
	CatalogKindOverage CatalogKind = "overage" // pri_… line-item (flat-rate per GB-hour)
	CatalogKindProduct CatalogKind = "product" // pro_… parent product
)

// CatalogEntry is the wire shape returned by ListCatalog. SyncedAt
// is the timestamp of the most recent successful EnsurePlanProducts
// call on this *Provider instance. Zero when no hydration has yet
// completed (a fresh boot with a sandbox-down Paddle returns a
// non-empty Entries list only after the SDK round-trip succeeds).
type CatalogEntry struct {
	Plan     api.Plan    `json:"plan"`
	Kind     CatalogKind `json:"kind"`
	Handle   string      `json:"handle"`
	SyncedAt time.Time   `json:"synced_at"`
}

// OpProvider is the interface the admin handler type-asserts against.
// Defined here in ops.go (not provider.go) so the dispatcher's
// runtime cost (one type assertion per call) is paid once, and so
// future providers that want a different operator surface can add
// their own interface without touching this file.
//
// All three methods must be safe to call concurrently with the
// meterd pusher + the changePlan handler. The Paddle implementation
// holds the catalog mutex for reads + writes; the Stripe provider
// does not implement this interface at all (handler returns 501).
type OpProvider interface {
	// ListCatalog returns the cached price + product handles from
	// the in-memory catalog. The SyncedAt field on each entry is
	// the timestamp of the most recent EnsurePlanProducts call.
	// Empty slice (never nil) when no hydration has yet succeeded.
	ListCatalog(ctx context.Context) []CatalogEntry

	// SyncCatalog forces a fresh EnsurePlanProducts round-trip and
	// returns the post-sync catalog entries. The list-then-create
	// loop in ensureProducts is idempotent on Paddle-side products
	// (matches on `faas-plan-<plan>` name prefix), so a redelivered
	// boot that calls this only walks the LIST endpoints. Operators
	// who reset their sandbox merchant use this to re-hydrate after
	// deleting the products from the Paddle Dashboard.
	SyncCatalog(ctx context.Context) ([]CatalogEntry, error)

	// ResetCatalog is a no-op on the *Provider side. The Paddle
	// catalog is durable on the platform; deleting the in-memory
	// cache here would not unlink the merchant's prices. The
	// operator must delete products via the Paddle Dashboard, then
	// call SyncCatalog to re-create. The CLI prints the warning.
	//
	// Returns nil always (the no-op succeeds) so the HTTP handler
	// can render a uniform "see CLI warning" UX. A future
	// implementation that supports merchant-side cleanup will
	// return an error from the SDK round-trip here.
	ResetCatalog(ctx context.Context) error
}

// Compile-time check that *Provider implements OpProvider. Adding a
// method to the interface without implementing it on *Provider is
// a build error here.
var _ OpProvider = (*Provider)(nil)

// ListCatalog returns the cached price + product handles, taken
// under the catalog RLock so concurrent SDK writes (via
// EnsurePlanProducts → ensureProducts) see a consistent point-in-time
// view. Returns an empty slice (never nil) when no hydration has
// yet succeeded; the JSON marshaler renders `[]` so the CLI can
// distinguish "never synced" from "synced but no entries" via the
// accompanying provider-status field.
func (p *Provider) ListCatalog(_ context.Context) []CatalogEntry {
	p.catalog.mu.RLock()
	syncedAt := p.lastSyncAt
	monthly := make(map[api.Plan]string, len(p.catalog.planMonthly))
	for k, v := range p.catalog.planMonthly {
		monthly[k] = v
	}
	overage := make(map[api.Plan]string, len(p.catalog.planOverage))
	for k, v := range p.catalog.planOverage {
		overage[k] = v
	}
	customers := make(map[api.Plan]string, len(p.catalog.planCustomers))
	for k, v := range p.catalog.planCustomers {
		customers[k] = v
	}
	p.catalog.mu.RUnlock()

	out := make([]CatalogEntry, 0, len(monthly)+len(overage)+len(customers))
	// Stable order: monthly, overage, product — matches the JSON the
	// admin endpoint renders. Within each kind, iterate api.Plans so
	// the CLI table is deterministic.
	for _, plan := range api.Plans {
		if h, ok := monthly[plan]; ok && h != "" {
			out = append(out, CatalogEntry{Plan: plan, Kind: CatalogKindMonthly, Handle: h, SyncedAt: syncedAt})
		}
	}
	for _, plan := range api.Plans {
		if h, ok := overage[plan]; ok && h != "" {
			out = append(out, CatalogEntry{Plan: plan, Kind: CatalogKindOverage, Handle: h, SyncedAt: syncedAt})
		}
	}
	for _, plan := range api.Plans {
		if h, ok := customers[plan]; ok && h != "" {
			out = append(out, CatalogEntry{Plan: plan, Kind: CatalogKindProduct, Handle: h, SyncedAt: syncedAt})
		}
	}
	return out
}

// SyncCatalog calls EnsurePlanProducts (which is already idempotent
// via the list-then-create loop in ensureProducts) and returns the
// post-sync catalog. The SDK round-trip is paid even when the cache
// is fresh — operators who call sync explicitly want to confirm
// the platform side is reachable, not just the in-memory cache.
//
// Errors from EnsurePlanProducts are wrapped with the operation
// name so an operator hitting the CLI sees "paddle: ensure plans:
// <sdk-error>" rather than a bare SDK string.
func (p *Provider) SyncCatalog(ctx context.Context) ([]CatalogEntry, error) {
	if err := p.EnsurePlanProducts(ctx); err != nil {
		return nil, err
	}
	return p.ListCatalog(ctx), nil
}

// ResetCatalog is a no-op for Paddle — the durable catalog lives on
// the platform, not in-memory. The CLI surfaces a warning; future
// work (issue #279+) may add merchant-side cleanup that returns an
// error here when the SDK call fails.
func (p *Provider) ResetCatalog(_ context.Context) error {
	return nil
}
