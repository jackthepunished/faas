# FaasTLSCertExpiryPage

Source: `deploy/ansible/roles/prometheus/files/faas.rules.yml`.
Metric: `gateway_tls_cert_expiry_seconds` (gatewayd `/metrics`).
Spec: §12 + ADR-024 H3 (closed in PR #345).
Severity: page.

## Symptom

Smallest remaining lifetime across cached certs in `cfg.StorageDir`
is ≤ 14 days. The §12 panel is the gauge the operator reads; the
alert pages the operator when the gauge drops below the hard 14-day
threshold. A negative value is also a page (cert already past
`NotAfter`).

The most likely cause is **certmagic renew failure** — DNS-01 writes
failed (Hetzner token rotated without the new one being loaded,
or the apps zone no longer grants zone-write to the token), or
certmagic's renew loop is wedged.

## Verify

```bash
curl -fsS http://127.0.0.1:9090/metrics | grep gateway_tls_cert_expiry_seconds
journalctl -u faas-gatewayd --since '-1h' --no-pager | grep -iE 'certmagic|renew|acme|hetzner'
ls -la /var/lib/faas/certs/certificates/  # 0700, faas:faas
```

## Check

```bash
# Is the Hetzner DNS token still valid? The error path emits
# "no such zone" or "unauthorized" on POST /api/v1/records.
journalctl -u faas-gatewayd --since '-24h' --no-pager | grep -iE 'hetzner|api/v1/records|unauthorized'

# Is the apps domain still delegated to this box's IP?
dig +short $(grep -E '^\[tls\]' -A 30 /etc/faas/gatewayd.toml | grep wildcard_cert_domain | head -1 | cut -d'"' -f2)
```

If the Hetzner token is the cause, rotate it:

1. Generate a new token in the Hetzner Cloud Console.
2. Drop the sealed token at `cfg.HetznerDNSAPITokenPath` (re-run
   `faas secrets seal` on the EX44; the LoadCredential call is
   what gatewayd reads).
3. `systemctl restart faas-gatewayd` (the H4 file-watch reload is
   the open follow-up; until then, a restart is the rotation step).
4. Certmagic will re-mint on the next wake. Watch the gauge
   climb back above 60 d.

## Silence

```bash
amtool silence add \
  --matchers='alertname=FaasTLSCertExpiryPage' \
  --duration=1h \
  --comment='Hetzner token rotation in progress'
```

## Recover

If certmagic's renew loop is wedged (logs show the same "obtaining
certificate" line for > 30 minutes with no progress), kill the
gatewayd; on restart certmagic re-evaluates against the on-disk
state and either resumes the in-flight obtain or starts fresh.
Re-verify the gauge climbs back above 60 d within 10 minutes of
the first mint.
