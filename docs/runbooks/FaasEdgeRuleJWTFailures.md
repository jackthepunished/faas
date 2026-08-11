# FaasEdgeRuleJWTFailures

Source: dashboard `faas-edge-rules-pr-b` panel 3
(`deploy/grafana/edge-rules.json`) — also surfaces as a Prometheus
alert rule in `deploy/ansible/roles/prometheus/files/faas.rules.yml`.
Metric: `gateway_edge_rule_match_total{kind="jwt",outcome="failed"}`
rate (single timeseries — the JWT outcome label set is **not** widened
in PR-B; the failure-reason breakdown is recoverable from the
`edge_rule.jwt_failed` audit row's `data.err` field).
Spec: §12 + ADR-091 Amendment 1.
Severity: warn.

## Symptom

Dashboard panel 3 yellow or red. The `match_total{kind="jwt",outcome="failed"}`
rate is > 5 / min for 5 minutes sustained. This is a **customer-facing
spike**: every tick is a request that reached the JWT gate and was
rejected, so the customer saw a 401.

> ⚠️ **CORS preflight short-circuits IP+JWT gates by design.** Per
> `pkg/gateway/handler.go::applyEdgeRuleCORS` ordering comments:
> "apply kind=cors preflight AFTER rewrite (so a rewritten path is
> matched against CORS rules) and AFTER headers (so request-side header
> ops don't shadow the preflight's Allow-* headers). CORS preflight
> short-circuits with 204 + Access-Control-Allow-* headers; the caller
> MUST return to skip the auth gates." Consequence: a JWT-failure
> spike dominated by `OPTIONS` method requests is **NOT a JWT problem**
> — it's a CORS preflight storm. Filter `r.Method != "OPTIONS"` before
> counting. If the panel fires but the audit grep below shows mostly
> `OPTIONS`, page the CORS runbook instead (forthcoming; see ADR-091
> Tier 2 unblocks).

## Verify

```bash
# The single timeseries that drives the panel:
curl -fsS http://127.0.0.1:9090/metrics | grep -E 'gateway_edge_rule_match_total\{.*kind="jwt",.*outcome="failed"'

# Method breakdown — confirm the spike is real JWT traffic, not
# CORS preflight short-circuit noise:
journalctl -u faas-gatewayd-internal --since '-10m' --no-pager \
  | grep 'edge_rule.jwt_failed' \
  | grep -oE 'method=[A-Z]+' \
  | sort | uniq -c | sort -rn
# If OPTIONS dominates, this is a CORS preflight storm — see call-out above.

# Audit-grep the failure reason — separates timeout from verifier error
# (per ADR-091 Amendment 1 §5: JWT outcome label widening was rejected
# in favour of an audit-grep).
journalctl -u faas-gatewayd-internal --since '-10m' --no-pager \
  | grep 'edge_rule.jwt_failed' \
  | grep -oE 'err=[^ ]+' \
  | sort | uniq -c | sort -rn
```

## Check

The `data.err` substring is the recovery-key discriminator:

- **`context deadline exceeded`** — the 5s JWT verify cap
  (`api.EdgeRuleJWTVerifyTimeoutDefault`) fired. The verifier was
  called but did not return in 5s. Most common cause: JWKS endpoint
  unreachable (network partition, IdP outage, DNS failure). Cross-check
  the customer's JWKS URL with `curl -fsS <jwks_url>` from the
  gatewayd-internal host.

- **signature mismatch / wrong algorithm** — the JWT's `alg` header
  doesn't match the rule's `algorithms` allow-list, or the signature
  doesn't verify against the JWKS. Most common cause: customer's
  identity provider rotated keys but the rule's `algorithms` list
  pinned to the old algorithm. Fix the rule.

- **wrong iss / wrong aud** — `iss` claim doesn't match the rule's
  `issuer`, or `aud` claim doesn't include the rule's `audience`.
  Customer-side misconfiguration (typo in the rule, or the customer's
  IdP is now issuing different values).

- **missing claim / expired exp** — the JWT is structurally valid but
  doesn't carry the required custom claim, or `exp` is in the past.
  Customer-side clock-skew (a few seconds of drift is fine; minutes
  points to an IdP clock problem).

```bash
# Pull the most recent jwt_failed audit rows in structured form:
journalctl -u faas-gatewayd-internal --since '-10m' --no-pager -o cat \
  | grep 'edge_rule.jwt_failed' \
  | tail -20
```

## Silence

```bash
amtool silence add \
  --matchers='alertname=FaasEdgeRuleJWTFailures' \
  --duration=1h \
  --comment='IdP outage in progress'
```

## Recover

1. **If the audit-grep shows mostly `context deadline exceeded`** —
   JWKS endpoint is unreachable. Page the customer's IdP / check
   network. The rule itself is fine; do not modify it. Recovery is
   automatic when the JWKS endpoint comes back.
2. **If the audit-grep shows verifier errors (signature / iss / aud /
   missing claim / expired)** — customer's IdP or the rule is
   misconfigured. Reach out to the customer's owner via the apid
   contact. Do **not** disable the rule without the customer's
   sign-off — disabling opens the endpoint to anonymous traffic.
3. **If the panel fires but the audit log shows mostly `OPTIONS`**
   — this is a CORS preflight storm, not a JWT problem. Ack the
   silence on `FaasEdgeRuleJWTFailures` and investigate the
   customer's CORS rules instead.