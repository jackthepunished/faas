# FaasEdgeRuleApplyHigh

Source: dashboard `faas-edge-rules-pr-b` panel 2
(`deploy/grafana/edge-rules.json`) — also surfaces as a Prometheus alert
rule in `deploy/ansible/roles/prometheus/files/faas.rules.yml`.
Metric: `gateway_edge_rule_apply_total{kind,result}` rate, per-kind
`result="error"`.
Spec: §12 + ADR-091 Amendment 1.
Severity: warn.

## Symptom

A per-kind `apply_total{result="error"}` rate > 1 / minute sustained over
5 minutes. Dashboard panel 2 paints red. Apply errors are non-2xx wire
writes — the customer saw a 4xx response, so this is a real user-facing
regression, not a headroom signal. Common culprits:

- **JWT** — verifier unreachable (JWKS endpoint down, IdP clock-skew,
  key rotation race). Bound: `api.EdgeRuleJWTVerifyTimeoutDefault = 5s`
  (`pkg/api/limits.go`) caps each call, so a hung JWKS endpoint surfaces
  as `data.err = "context deadline exceeded"` in the
  `edge_rule.jwt_failed` audit row.
- **IP** — CIDR off-by-one in a deny or allow list. The
  `edge_rule.ip_blocked` audit row's `cidr` field carries the matched
  rule's CIDR; the request's `client_ip` field carries the request's
  source IP. Cross-check these to spot the typo.
- **CORS** — customer's preflight is failing their own allow-origin
  (mismatch between the rule's `allow_origins` and what the
  customer's `Origin` header actually sends).

## Verify

```bash
# What's the per-kind error rate right now?
curl -fsS http://127.0.0.1:9090/metrics | grep -E 'gateway_edge_rule_apply_total{.*result="error"'

# Which kind(s) are firing?
curl -fsS http://127.0.0.1:9090/metrics | grep -E 'gateway_edge_rule_apply_total{.*result="error"' | awk -F'kind="' '{print $2}' | awk -F'"' '{print $1}' | sort -u

# Audit context — what rule ID + source IP are involved?
journalctl -u faas-gatewayd-internal --since '-10m' --no-pager \
  | grep -E 'edge_rule\.(jwt_failed|ip_blocked|caller_ip_forged|cors_rejected)'
```

## Check

- **JWT errors** — `journalctl -u faas-gatewayd-internal --since '-5m'
  --no-pager | grep edge_rule.jwt_failed`. Check
  `pkg/edgejwks.DefaultFetchTimeout` (5s) and the customer's JWKS
  endpoint reachability. If JWKS endpoint is up and the rule's `jwks_url`
  looks right, the audit's `data.err` substring tells you:
  - `context deadline exceeded` → timeout (network or upstream JWKS slow)
  - other → verifier logic (signature mismatch, wrong `iss`, wrong `aud`,
    expired `exp`, missing claim)
- **IP errors** — `journalctl -u faas-gatewayd-internal --since '-5m'
  --no-pager | grep edge_rule.ip_blocked`. Each audit row carries
  `cidr`, `client_ip`, `rule_id`. A pattern of `cidr` very close to a
  real customer IP range (e.g. off-by-one in the last octet) points
  to a typo. The `caller_ip_forged` kind means an `X-Forwarded-For`
  forgery attempt was rejected — see `FaasEdgeRuleJWTFailures.md` and
  PR-C for the XFF handling story.
- **CORS errors** — `journalctl -u faas-gatewayd-internal --since '-5m'
  --no-pager | grep edge_rule.cors_rejected`. Each row carries
  `request_origin`, `allow_origins` (the rule's set), and `rule_id`.
  Most customer-reported CORS errors are origin mismatches, not bugs.

## Silence

```bash
amtool silence add \
  --matchers='alertname=FaasEdgeRuleApplyHigh' \
  --duration=2h \
  --comment='investigating gate regression'
```

## Recover

Identify the offending rule from the audit log (cross-reference
`rule_id` and `app_id`), then either:

1. Fix the rule in apid (e.g. correct the CIDR, add the missing
   `jwks_url`, expand `allow_origins`).
2. Disable the rule (`enabled = false`) as a stop-gap; traffic resumes
   at the next apply call without the rule's effect.
3. For JWT timeouts specifically: check the customer's IdP for an
   outage before touching the rule — most JWT-failed spikes are IdP
   outages, not rule bugs.