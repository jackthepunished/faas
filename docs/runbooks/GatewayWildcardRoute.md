# GatewayWildcardRoute

**Severity:** info
**Component:** gatewayd-internal
**Family:** route_metrics
**ADR:** ADR-093

## Meaning

A single app per-route histogram has been emitting requests with route='__route_other__' at more than 1 reqps for 10 minutes. The bounded admission set (50 distinct real routes per app, ADR-093 D2) is collapsing distinct paths because that app is generating more than 50 distinct routes.

The customer app still serves correctly — the operator is losing per-route granularity on it, not correctness. The alert is the cue to ask the customer which it is.

## Two interpretations

1. Wildcard route pattern — /users/{uuid} style, legit >50 distinct paths.
2. Path-fuzzing or scrape — bot hitting many paths looking for a misconfiguration.

## Triage

1. Confirm the alert is firing on a single app tuple:
   promtool query instant http://localhost:9090 'sum by (app, route) (rate(gateway_requests_total{route="__route_other__"}[5m]))'
2. Pull path distribution via control listener:
   curl http://127.0.0.1:9090/v1/internal/apps/<slug>/routes
3. Verify apps.route_metrics_enabled=true on the affected app.

## Mitigation

Customer's call — accept collapse OR add rate-limit/WAF rules. Operator can lower pkg/api/limits.go RouteMetricsPerAppCap to fire earlier. Do not raise the cap.

## Long-term

Follow-up: per-app cap configurable on apps.route_metrics_cap (out of scope here).

## Related

- docs/adr/093-per-route-app-metrics.md
- docs/adr/042-per-app-metrics-and-cold-boot-rename.md
- pkg/gateway/route_label_set.go
- pkg/api/limits.go (RouteMetricsPerAppCap)
