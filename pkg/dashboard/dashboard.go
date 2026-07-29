// Package dashboard holds the server-rendered dashboard surface that
// apid exposes for M7.5 (ADR-011). The decision is to ship a thin Go
// html/template + HTMX shell — no SPA build chain, no JS framework —
// so the whole funnel fits inside the 6 GB control-plane slice
// (spec §13). gatewayd reverse-proxies /dashboard/* and /oauth/* to
// apid's loopback listener so the §11 single-public-listener invariant
// stays intact (ADR-011).
//
// Templates live under templates/ and are baked into the binary via
// embed.FS so deploys ship one artefact per daemon (CLAUDE.md).
// Slice 2 ships just enough to prove the surface renders; slice 4
// fills in the real data.
package dashboard

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/onebox-faas/faas/pkg/state"
)

//go:embed templates/*.html
var tmplFS embed.FS

// Page is the data each dashboard handler hands to Render. Keeping it
// in this package (not pkg/api) means dashboard handlers don't grow
// new fields in the public DTO surface by accident.
type Page struct {
	// Title is the <title> tag content.
	Title string
	// Flash is a one-line status banner rendered above the page body.
	// Used by /login and /auth/verify to surface "check your email".
	Flash string
	// Account is the signed-in account for authed pages. nil for
	// /login + /auth/*; dashboardAuth rejects unauthed requests for the
	// rest of /dashboard/*.
	Account *AccountView
	// Nonce is the per-request CSP nonce minted by the
	// httpsec.Nonce middleware (cmd/apid/server.go). Render stamps it
	// onto every <script> and <style> tag inside the rendered HTML so
	// the browser accepts the inline code under the page's
	// Content-Security-Policy header. Empty string is tolerated (the
	// template renders the tags without a nonce attribute) so unit
	// tests don't need to mint a nonce; production always sets it.
	Nonce string
	// Body is the per-page template name (without the .html suffix).
	// Render looks up templates/<Body>.html inside the layout.
	Body string
	// Data is the page-specific payload (struct, map, etc.).
	Data any
}

// AccountView is the dashboard-facing slice of state.Account. Never
// log secrets here. Source data is pkg/state.Account; slices 3+4 expand.
type AccountView struct {
	ID       string
	Email    string
	Plan     string
	AppCount int
}

// DPAView is the page-specific payload for the dashboard DPA route
// (/dashboard/account/dpa). Markdown is the rendered DPA plaintext
// the apid main passes in after reading the configured dpaPath file.
// Keeping the body opaque here means a future maintainer can swap
// the markdown processor (goldmark, blackfriday, etc.) without
// touching the page template.
type DPAView struct {
	Markdown string
}

// IndexData is the /dashboard/ overview payload.
type IndexData struct {
	DeployedAppCount int
	Plan             string
}

// AppListItem is one row on /dashboard/apps.
type AppListItem struct {
	Slug         string
	Status       string
	URL          string
	LastDeployed string // empty when no deploys yet
	// StateBadge* is the cold-wake status glyph ux_spec §6.3 asks
	// for: ● running / ◌ sleeping / ⟳ waking / · idle (failed/
	// stopped). Set by renderAppsList via BadgeFor based on the
	// newest instance row for the app; "" until the dashboard
	// plumbs it (kept on the type so templates can render an empty
	// glyph during partial migrations).
	StateBadge      string
	StateBadgeGlyph string
	StateBadgeLabel string
}

// ManifestView is the runner-scaffold snapshot shown on the app detail
// page. Names are JSONish to avoid a second copy of pkg/api.AppManifest.
type ManifestView struct {
	Entrypoint []string
	Env        map[string]string
	WorkingDir string
	Port       int
	Healthz    string
	User       string
}

// DeploymentItem is one row on the app detail page's deploy list.
type DeploymentItem struct {
	ID        string
	Status    string
	Kind      string
	CreatedAt string
	Error     string
}

// CronItem is one row on the app detail page's crons tab.
type CronItem struct {
	ID          string
	Schedule    string
	Path        string
	Enabled     bool
	LastFiredAt string // empty until first fire
}

// AppDetailData combines the bits the app detail page renders.
type AppDetailData struct {
	App         AppListItem
	Manifest    ManifestView
	Deployments []DeploymentItem
	Crons       []CronItem
	// RecentInstances is the most recent N wake rows for this app
	// (parked → waking → running → …). Each carries its WakeID so
	// operators can paste the ID from a gateway response header
	// (`x-faas-wake-id`) and find which run it was. The list is
	// derived from ListInstancesForApp ordered DESC by started_at;
	// if the store returns no rows (cold-install, fully parked),
	// the section renders as empty and the template shows a hint.
	RecentInstances []RecentInstanceItem
	// Metrics is the per-app Prometheus snapshot (issue #273 / ADR-042).
	// nil = skip the section entirely (Prometheus not configured, or
	// the fetch failed non-fatally). When non-nil with RequestCount=0
	// the template renders an "no requests in this window" empty
	// state instead of a row of zeros. Source is the same
	// "prometheus" / "degraded: <err>" vocabulary the public status
	// page uses so the dashboard has one empty-state path.
	Metrics *AppMetricsView
	// Alerts is the per-app (and account-wide) alert-rule snapshot
	// (issue #396 / ADR-045, PR 4). nil means the apid dashboard
	// query failed non-fatally (the page renders the "Alerts"
	// section as a warning); an empty slice renders the empty-state
	// line. RecentDeliveries per rule is capped at 5 by the handler.
	Alerts *AlertDetailData
}

// AlertDetailData is the dashboard-facing payload for the alert-rules
// panel. The shape matches what pkg/dashboard/templates/app_alerts.html
// renders — one row per rule with up to 5 most-recent deliveries.
// FailureSource / Metric / Comparison / Threshold / WindowSpec are
// already strings in state.AlertRule (closed vocabularies) so the
// template renders them directly; no enum mapping needed on this
// surface (the API DTO side does the normalisation per PR 3).
type AlertDetailData struct {
	// Rules is the alert-rule list scoped to the current app +
	// account-wide. The handler filters state.Store.ListAlertRulesForAccount
	// down to rule.AppID == app.ID || rule.AppID == "" — the same
	// visibility filter the public API uses (handlers_alerts.go).
	Rules []AlertItem
}

// AlertItem is one row on the dashboard's "Alerts" panel. RecentDeliveries
// is rendered underneath the rule row so operators see the last 5
// dispatch attempts without leaving the page. Status on AlertDelivery
// is "delivered" / "failed" / "pending"; LastError is truncated at
// the handler edge so a 32 KiB SSRF-rejected URL doesn't blow the
// dashboard layout.
type AlertItem struct {
	Rule             state.AlertRule
	RecentDeliveries []state.AlertDelivery
	// LastFiredAtLabel is a pre-formatted relative timestamp
	// ("3m ago" / "just now" / "—") computed at the handler edge so
	// the template stays a pure renderer. Empty until LastFiredAt
	// goes non-zero.
	LastFiredAtLabel string
}

// alertDeliveryErrorLimit caps the LastError string we render on the
// dashboard so a rejected SSRF URL ("http://10.0.0.1:8080/...: egress
// denied: …") doesn't blow the panel column width.
const alertDeliveryErrorLimit = 200

// FormatAlertError trims LastError to alertDeliveryErrorLimit bytes.
// The handler applies this before handing to the template; the
// helper is exported because cmd/apid/handlers_dashboard.go is
// outside pkg/dashboard and needs to reach it. Kept as a thin
// formatter rather than a method on AlertDelivery so the truncation
// policy is testable in isolation (pkg/dashboard/dashboard_test.go).
func FormatAlertError(s string) string {
	if len(s) <= alertDeliveryErrorLimit {
		return s
	}
	return s[:alertDeliveryErrorLimit-1] + "…"
}

// RelativeTime labels a timestamp with a coarse "just now / Nm ago /
// Nh ago" string suitable for the dashboard's "Last fired" column.
// Negative diffs (clock skew) render as "just now" rather than
// "<future>". Exported for the same reason as FormatAlertError;
// cmd/apid/handlers_dashboard.go applies it on the data-loader side
// before the template renders.
func RelativeTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d / time.Minute)
		return fmt.Sprintf("%dm ago", m)
	default:
		h := int(d / time.Hour)
		if h < 48 {
			return fmt.Sprintf("%dh ago", h)
		}
		return t.UTC().Format("2006-01-02")
	}
}

// AppMetricsView is the dashboard-facing snapshot of one app's
// metrics (issue #273 / ADR-042). Numbers are pre-formatted; the
// template renders them via plain {{printf "%g" .}}. P50/P95/P99 are
// the 2xx-class only — failures surface as ErrorRatePct. WakeP95MS is
// the FLEET p95; the dashboard template labels it as such.
type AppMetricsView struct {
	Range        string // echoed window, e.g. "5m"
	Source       string // "prometheus" / "degraded: <reason>"
	RequestCount int64
	LatencyP50MS float64
	LatencyP95MS float64
	LatencyP99MS float64
	ErrorRatePct float64
	ColdStartPct float64
	WakeP95MS    float64
}

// RecentInstanceItem is one row of the Recent Wakes table on the
// dashboard app-detail page.
type RecentInstanceItem struct {
	ID            string // instance row PK (stable across wakes)
	WakeID        string // per-wake UUIDv7; distinct from ID
	State         string // wire vocabulary; the template badge maps parked → sleeping
	StartedAt     string // empty when not yet started
	LastRequestAt string // empty when no traffic yet
}

// UsageData is the /dashboard/usage page payload.
type UsageData struct {
	Month           string
	UsedGBHours     float64
	IncludedGBHours int64
	OverageGBHours  float64
	UsedPct         float64 // 0..100+
	// UsedEgressGB (ADR-046, step 10) is the per-month
	// informational egress roll-up (Σ tx_bytes +
	// net_tx_bytes across all apps). Not billed; the
	// template renders it next to the GB-h panel as a
	// "this much egress" line. The gateway-side tx_bytes
	// producer lands in PR-2; until then the value is
	// 0 because NetTxBytes is the only source populated.
	UsedEgressGB float64
}

// BillingData is the /dashboard/billing page payload.
type BillingData struct {
	Plan     string
	RAMMB    int
	Included int64
	AppsCap  int
	AppLayer int
	IdleSec  int
}

// PricingData is the /dashboard/pricing page payload (issue #259).
// Plans is the four-plan table authoritative from pkg/api/limits.go;
// Highlighted marks the row that matches the current account's plan
// so the dashboard can render a "Your plan" badge. PriceFormatted is
// the on-the-wire "€X.YY" / "Free" string — money stays integer
// millicents upstream and is divided into euros at template time only.
type PricingData struct {
	Plans []PricingPlanView
}

// PricingPlanView is one row on /dashboard/pricing. Every field is
// derived from api.Limits — never inline a quota here, the limit
// table is the single source of truth (CLAUDE.md Hard limits).
type PricingPlanView struct {
	Plan                    string
	PriceFormatted          string
	Highlighted             bool
	DeployedApps            int
	MaxConcurrency          int
	RAMMB                   int
	AppLayerMaxMB           int
	SourceTarballMaxMB      int
	IdleTimeoutS            int
	IncludedGBHours         int64
	RateLimitRPS            int
	RateLimitBurst          int
	EgressMbit              int
	SecretCountMax          int
	AsyncInvokeAllowed      bool
	MinInstancesAllowed     bool
	ScaleUpTargetRPSAllowed bool
	ScaleUpTargetCPUAllowed bool
	EgressAllowlistAllowed  bool
	EgressAllowlistMaxSize  int
}

// InvoicesData is the /dashboard/invoices page payload (issue #259).
// Items is one row per invoice; NextBefore is the RFC3339Nano cursor
// for the older page (empty when this is the end). Month is the
// currently-applied filter, echoed back so the template can
// pre-fill the form input.
type InvoicesData struct {
	Month      string
	Items      []InvoiceRow
	NextBefore string
}

// InvoiceRow is one dashboard row. TotalFormatted is pre-formatted at
// the handler edge (integer cents → "€X.YY" / "€0.00"; never float).
// PDFAvailable shows the marker (Y / -) but never exposes the provider
// PDF URL. HostedURL is intentionally absent: the column lives in
// invoices.hosted_url for PR-B audit only; PR A never puts it on the
// wire (see state.Invoice docstring).
type InvoiceRow struct {
	ID             string
	Number         string
	Provider       string
	Status         string
	Period         string // "2026-07"
	TotalFormatted string // "€X.YY" — pre-formatted at the handler edge
	Currency       string
	PDFAvailable   bool
}

// APIKeyItem is one row on the /dashboard/account page's keys tab.
//
// Scopes is the fine-grained permission set the apid IAM-1 (ADR-034
// rev2) vocabulary grants the key (e.g. ["admin"], ["apps:read",
// "deploy:write"], or the legacy single-element form). The dashboard
// template renders the slice as a comma-separated list. Defaults to
// empty for older accounts that pre-date the migration's CHECK
// constraint guarantee (the renderer treats nil as "—" — see
// account.html).
type APIKeyItem struct {
	ID         string
	Prefix     string
	Label      string
	Scopes     []string
	CreatedAt  string
	LastUsedAt string // empty until first use
}

// AccountData is the /dashboard/account page payload.
type AccountData struct {
	Keys []APIKeyItem
	// ShowDelete + DeleteConfirmToken drive the "Danger zone" partial
	// in templates/account.html. The token is a sealed envelope bound
	// to (action="delete", account_id) that the POST handler verifies
	// via pkg/middleware.VerifyAuthenticated. The matching faas_csrf
	// sidecar cookie is set by the renderer (handlers_dashboard.go)
	// so a cross-site POST cannot read it. ShowRestore +
	// RestoreUntil render the matching "Restore account" form when
	// the row is in deleted_pending — the deadline is the human-
	// readable restore_until the dashboard template surfaces.
	ShowDelete          bool
	DeleteConfirmToken  string
	ShowRestore         bool
	RestoreUntil        string
	RestoreConfirmToken string
	// FlashSurface holds "scheduled for deletion" / "restored" banners
	// the dashboard reads from ?deleted=1 / ?restored=1 in the URL.
	// Kept here (not Page.Flash) so the danger-zone partial stays a
	// self-contained block the layout file can render unconditionally.
	FlashSurface string
}

// Render writes the page to w. It parses the templates on first use
// and caches the parsed tree in a sync.Once — the hot path is a
// single Execute call.
//
// nonce is the per-request CSP nonce minted by httpsec.Nonce
// (cmd/apid/server.go). Render stamps it onto page.Nonce so every
// <script> and <style> tag inside the layout and body template can
// emit `nonce="…"` via {{.Nonce}}. Empty nonce is allowed (unit
// tests pass "") and the templates render the tags without a nonce
// attribute — production always supplies a value via
// httpsec.NonceFromContext.
//
// Body is the per-page template name ("index", "apps_list", …). It
// MUST be present under templates/ or Render writes a 500 problem.
func Render(w http.ResponseWriter, log *slog.Logger, nonce string, page Page) error {
	page.Nonce = nonce
	if page.Body == "" {
		page.Body = "index"
	}
	t, err := parseTemplates()
	if err != nil {
		log.Error("dashboard template parse failed", "err", err)
		return err
	}
	tplName := page.Body + ".html"
	if t.Lookup(tplName) == nil {
		log.Error("dashboard template missing", "name", page.Body)
		return fmt.Errorf("dashboard: template %q not found", page.Body)
	}
	var buf bytes.Buffer
	// Execute the page template directly with the Page struct. Each
	// page template defines the full <html>…</html> wrapper (not a
	// shared layout) — slices stay small enough that the duplication
	// is cheaper than a layout-include dance.
	if err := t.ExecuteTemplate(&buf, tplName, page); err != nil {
		log.Error("dashboard template execute failed", "err", err, "page", page.Body)
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err = buf.WriteTo(w)
	return err
}

var (
	tmplOnce sync.Once
	tmplTree *template.Template
	tmplErr  error
)

func parseTemplates() (*template.Template, error) {
	tmplOnce.Do(func() {
		// Parse the layout + every page so {{template "name"}} lookups
		// resolve inside the layout. Failure is fatal for the daemon.
		tmplTree, tmplErr = template.New("").ParseFS(tmplFS, "templates/*.html")
	})
	return tmplTree, tmplErr
}
