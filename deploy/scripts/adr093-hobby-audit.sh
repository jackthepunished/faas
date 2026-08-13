#!/usr/bin/env bash
# adr093-hobby-audit.sh — Hobby-tier app audit harness for ADR-093.
#
# Tier A operational follow-up after PR #856 (ADR-093 deploy rollout).
# Empirical answer to "is the 50-route cap (ADR-093 D2) actually
# bounding cardinality, or do Hobby-tier apps immediately collapse
# into the __route_other__ overflow bucket?"
#
# This script is READ-ONLY:
#   - GET /v1/admin/obs/tenants?plan=hobby  (operator endpoint)
#   - GET /v1/apps/{slug}/routes           (per-route reader)
#
# It does NOT flip apps.route_metrics_enabled=true on any Free app
# (Free apps gate out via Plan.RouteMetricsResponseAllowed() at
# pkg/api/limits.go:2662-2668, enforced in cmd/apid/handlers_ext.go:
# 279-286 with code plan_route_metrics_not_allowed).
#
# Outputs a markdown table to stdout (or --output FILE). Pipe to
# `tee` to log to the deploy record or the docs/STATUS.md "Tier A"
# closing entry.
#
# Usage:
#   FAAS_API_BASE=https://api.gregale.dev \
#   FAAS_TOKEN=faas_pat_... \
#   ./deploy/scripts/adr093-hobby-audit.sh
#
#   # or:
#   FAAS_API_BASE=https://api.gregale.dev FAAS_TOKEN=... \
#   ./deploy/scripts/adr093-hobby-audit.sh --output hobby-audit.md
#
# Environment:
#   FAAS_API_BASE    Required. The apid base URL (no trailing slash).
#   FAAS_TOKEN       Required. Bearer token with operator scope
#                    (admin/obs/tenants requires operator; per-route
#                    reader requires ScopesReadSurface on the app's
#                    owning tenant).
#   FAAS_BOX_DOMAIN  Optional. Only used in the host column of the
#                    output table for clarity. Defaults to the host
#                    of FAAS_API_BASE.
#
# Requires:
#   bash ≥ 4 (associative arrays), curl, jq. All three are in the
#   bootstrap toolchain per deploy/ansible/roles/bootstrap/tasks/main.yml.

set -euo pipefail

# --- 0. Args + env --------------------------------------------------------

OUTPUT="${FAAS_OUTPUT:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output) OUTPUT="$2"; shift 2 ;;
    --help|-h)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[[ -n "${FAAS_API_BASE:-}" ]] || { echo "FAAS_API_BASE not set" >&2; exit 2; }
[[ -n "${FAAS_TOKEN:-}"    ]] || { echo "FAAS_TOKEN not set" >&2;    exit 2; }
command -v curl >/dev/null || { echo "curl not on PATH" >&2; exit 2; }
command -v jq   >/dev/null || { echo "jq not on PATH"   >&2; exit 2; }

# Strip trailing slash for clean URL concatenation.
FAAS_API_BASE="${FAAS_API_BASE%/}"

BOX="${FAAS_BOX_DOMAIN:-$(printf '%s' "$FAAS_API_BASE" | sed -E 's#^https?://##; s#/.*##')}"

heading() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }
ok()      { printf '\033[1;32m✓\033[0m %s\n' "$*"; }
warn()    { printf '\033[1;33m!\033[0m %s\033[0m\n' "$*" >&2; }
fail()    { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# --- 1. Sanity: list the Hobby-tier tenants -------------------------------

heading "1/3 listing Hobby-tier tenants"

# The operator endpoint returns rows like:
#   {"tenant_id": "...", "slug": "...", "plan": "hobby", ...}
# See cmd/apid/handlers_admin_obs.go:133-167 obsListTenants +
# handlers_admin_obs_projection.go:143-161 filterTenantRows.
TENANTS_JSON=$(curl -fsS \
  -H "Authorization: Bearer $FAAS_TOKEN" \
  "$FAAS_API_BASE/v1/admin/obs/tenants?plan=hobby&limit=200") \
  || fail "GET /v1/admin/obs/tenants?plan=hobby failed"

TENANT_COUNT=$(printf '%s' "$TENANTS_JSON" | jq -r '.data // .tenants // [] | length')
[[ "$TENANT_COUNT" -gt 0 ]] || fail "no Hobby-tier tenants found"
ok "$TENANT_COUNT Hobby-tier tenant(s)"

# --- 2. Per-tenant: enumerate apps + per-route reader ---------------------

heading "2/3 enumerating apps + per-route readers"

# Output rows accumulate into a TSV that we render as markdown at the end.
declare -a ROWS=()
# Header: tenant_slug, app_slug, route_count, real_routes, overflow_routes, status
ROWS+=("tenant|app|routes|real|overflow|status")

# For each Hobby tenant, list apps, then for each app GET /routes.
while IFS=$'\t' read -r tenant_slug app_slug; do
  [[ -z "$tenant_slug" || -z "$app_slug" ]] && continue

  ROUTES_JSON=$(curl -fsS \
    -H "Authorization: Bearer $FAAS_TOKEN" \
    "$FAAS_API_BASE/v1/apps/$app_slug/routes" 2>/dev/null) \
    || { warn "GET /v1/apps/$app_slug/routes failed for tenant=$tenant_slug — skipping"; continue; }

  # Empty / missing 'routes' array means the app's plan_gate is off
  # (Free PATCHed to true would 403 plan_route_metrics_not_allowed;
  # this is read-only and we already filter to Hobby in step 1).
  ROUTE_COUNT=$(printf '%s' "$ROUTES_JSON" | jq -r '.routes // [] | length')
  if [[ "$ROUTE_COUNT" == "0" || "$ROUTE_COUNT" == "null" ]]; then
    ROWS+=("$tenant_slug|$app_slug|0|0|0|empty")
    continue
  fi

  # Count __route_other__ entries (the overflow bucket at
  # pkg/gateway/route_label_set.go:57).
  OVERFLOW=$(printf '%s' "$ROUTES_JSON" \
    | jq -r '[.routes[] | select(.route=="__route_other__")] | length')
  REAL=$((ROUTE_COUNT - OVERFLOW))

  STATUS="ok"
  if [[ "$ROUTE_COUNT" -ge 50 ]]; then
    STATUS="saturated"
  elif [[ "$OVERFLOW" -gt 0 ]]; then
    STATUS="overflowing"
  fi

  ROWS+=("$tenant_slug|$app_slug|$ROUTE_COUNT|$REAL|$OVERFLOW|$STATUS")
done < <(printf '%s' "$TENANTS_JSON" \
  | jq -r '.data // .tenants // [] | .[] | .slug as $t | (.apps // []) | .[]? | [$t, .] | @tsv')

# --- 3. Render markdown table ---------------------------------------------

heading "3/3 results"

TABLE=$(printf '%s\n' "${ROWS[@]}" | awk -F'|' '
  BEGIN { print "| Tenant | App | Routes | Real | Overflow | Status |"
          print "|--------|-----|-------:|-----:|---------:|--------|" }
  NR==1 { next }
  { printf "| %s | %s | %s | %s | %s | %s |\n", $1, $2, $3, $4, $5, $6 }
')

if [[ -n "$OUTPUT" ]]; then
  {
    printf '# ADR-093 Hobby-tier audit — %s\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'box: `%s`  \n' "$BOX"
    printf 'api: `%s`  \n' "$FAAS_API_BASE"
    printf 'cap: `50` (ADR-093 D2, `pkg/api/limits.go` `RouteMetricsPerAppCap`)\n\n'
    printf 'Status legend: **saturated** = 50 admitted (cap hit), **overflowing** = __route_other__ > 0 but below cap, **ok** = below cap and no overflow, **empty** = no routes in response.\n\n'
    printf '%s\n' "$TABLE"
  } > "$OUTPUT"
  ok "wrote $OUTPUT"
else
  {
    printf '# ADR-093 Hobby-tier audit — %s\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'box: `%s`  \n' "$BOX"
    printf 'api: `%s`  \n' "$FAAS_API_BASE"
    printf 'cap: `50` (ADR-093 D2, `pkg/api/limits.go` `RouteMetricsPerAppCap`)\n\n'
    printf 'Status legend: **saturated** = 50 admitted (cap hit), **overflowing** = __route_other__ > 0 but below cap, **ok** = below cap and no overflow, **empty** = no routes in response.\n\n'
    printf '%s\n' "$TABLE"
  }
fi

# Exit non-zero if any saturated rows. This makes the script
# cron-safe: a non-zero exit signals "Tier B follow-up is warranted".
SATURATED=$(printf '%s\n' "${ROWS[@]:1}" | awk -F'|' '$6=="saturated"' | wc -l | tr -d ' ')
if [[ "$SATURATED" -gt 0 ]]; then
  warn "$SATURATED app(s) saturated — see Tier A observation period docs/STATUS.md decision tree"
  exit 1
fi

ok "no Hobby-tier apps saturated — cap is holding"