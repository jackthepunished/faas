# FaasDebugRegressionDisabledByOperator

Source: `deploy/ansible/roles/prometheus/files/faas.rules.yml`.
Metric: `apid_debug_regression_skipped_flag_disabled_total`
(counter). ADR: ADR-127 PR-B (production debugger consumer
surface). Severity: info, 1h `for:` window. No page — this
alert surfaces the operator's deliberate opt-out from the
regression cron so a customer "my regression banner is gone"
ticket can be triaged without paging on-call.

## Symptom

The per-app regression detector (cmd/apid/debug_regression_cron.go::
startDebugRegressionCron) is being skipped on every 5-minute tick
because the operator has set `FAAS_REQUEST_TELEMETRY_ENABLED` to
a falsy value (0/false/no/off). The cron bumps
`apid_debug_regression_skipped_flag_disabled_total` once per
skipped pass. The alert expression
`increase(apid_debug_regression_skipped_flag_disabled_total[1h]) > 0`
fires when the rate is non-zero for 1h.

This alert is **informational**: the operator may have
deliberately silenced the cron during a debug session, a
planned outage drill, or a customer incident where the cron
itself is generating the alert. The 1h `for:` window is
deliberately loose so a single deliberate opt-out doesn't page
on-call but sustained opt-outs show up in the dashboard.

## Triage

1. **Confirm the operator's intent**: check the deploy log /
   change-management channel for a recent set of
   `FAAS_REQUEST_TELEMETRY_ENABLED=false` on the apid service.
   This is the deliberate-opt-out signal — the alert is doing
   its job by surfacing the choice.
2. **Confirm the alert's stable-time window**: if the rate is
   positive for <1h (the alert's `for:` window), the operator
   flipped and flipped back — that's not a customer-impacting
   state. Page only on sustained opt-outs.
3. **Open a regression-detection-vacuum issue**: if the
   opt-out has been in place for >24h (a holiday, a customer
   escalation that the team is debugging), open a follow-up
   ticket to remind the team that
   `debug_regression_observations` is no longer being refreshed.
   The dashboard's regression banner will appear empty even on
   accounts that DO have a regression — a regression detection
   vacuum is silent but it can mask a real shipping regression.

## Resolution

- The operator's change-management record should reflect the
  opt-out start/end timestamps. Restore by unsetting
  `FAAS_REQUEST_TELEMETRY_ENABLED` and restarting apid; the
  loop respawns on the next process startup.
- Confirm the alert cleared within 1h of the unset (the
  counter stops incrementing when the loop resumes).
- If the alert kept firing after the unset, the operator may
  have set the env var through multiple channels (systemd
  drop-in + sealed.env). Check both
  `/etc/faas/secrets/apid/sealed.env` and the systemd unit
  `Environment=` line, plus any helm/kustomize overlay that
  sets the apid pod env.

## When NOT to escalate

- A single "increase > 0" in the 1h window right after a
  deploy — that's the operator's audit trail showing they set
  the opt-out during the deploy, which is normal. The `for:`
  window holds the page back; this is by design.
- The alert clears within 1h — that was an experiment or a
  rollback test.

## Reference

The opt-out is encoded at
`cmd/apid/debug_regression_cron.go::startDebugRegressionCron`:
```go
enabled := func() bool {
    return getenv("FAAS_REQUEST_TELEMETRY_ENABLED") != "false"
}
```

Any falsy value (0/false/no/off; case-insensitive) silences
the cron. The default (env unset) leaves the cron enabled —
the runbook assumes the unset case is the production posture.

The corresponding `kill-switch` for the upstream recorder
(lives in pkg/gateway/request_telemetry.go) uses a different
env var: `FAAS_DEBUG_TELEMETRY_GATE` on the gatewayd-internal
host. If the recorder is also off, the regression cron has no
data to operate on; the empty banner is expected.
