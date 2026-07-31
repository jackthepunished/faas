# Host age rotation

The X25519 host identity at `/etc/faas/secrets/host.age`
(mode `0400 root:root`, spec §11) seals every customer-visible secret
on the box: per-app `app_secrets` envelopes (apid writes),
per-instance TOTP MFA secrets (apid reads via
`LoadCredential=faas_host_age_identity`), per-wake secret env vars
(vmmd reads), githubd install tokens, alert evaluator webhook
secrets. Every customer envelope is sealed to the host's
**current** age recipient; if the on-disk key changes without
plumbing to keep envelopes sealed under the **previous** key
unsealed, every MFA confirm + every wake secret injection +
every githubd install-token rehydrate 5xx's on the same instant.

This runbook is the rotation procedure. It is **not** a scheduled
cadence — host.age rotation is an incident-response or
policy-compliance event, not a 90-day recurring task. Issue #316 /
ADR-057.

## Threat model

The host identity is the root of the box's trust tree. Rotation
matters when:

1. **Identity compromise.** An attacker has read the on-disk
   `host.age` (0400 root:root implies root compromise — that's a
   Tier-0 event, but rotation is still the recovery step).
2. **Key age / policy.** Some compliance frameworks (SOC2 CC6.1,
   PCI-DSS 3.6.4) bound the on-disk age of cryptographic material.
   The one-box FaaS doesn't have a scheduled cadence, but if a
   future audit demands one, this is the procedure.
3. **Forward secrecy hygiene.** Even without compromise, rotating
   every N years caps the blast radius of any latent read event
   (e.g. a snapshot that was exfiltrated but never decrypted until
   later).

What rotation does NOT do:

- It does NOT re-seal pre-rotation envelopes to the new key.
  Envelopes sealed under the previous key remain sealed under the
  previous key; only the unseal side learns the new identity, via
  the 30-day overlap window.
- It does NOT change `audit-HMAC` values. The audit-join key is
  independently generated (`/var/lib/faas/audit-hmac.key`,
  0600 root:root — see `docs/ops/secrets-rotation.md`) and stable
  across host.age rotation. `events.data.email_hash` for the same
  email is identical before and after a host.age rotation.
- It does NOT change customer app secrets. `app_secrets` rows
  remain under the recipient they were sealed with; the
  unseal-side identity set just grows to cover both keys during
  the overlap.

## v1 partial-deliverable

This runbook ships the 30-day overlap path. The full re-seal
flow (background daemon that re-seals every `app_secrets` row
from the previous key to the current key, gated by a config knob)
is filed as `issue-316-followup-rekey` and is **not** in this PR.
For a single-box deployment the overlap window is sufficient:
no operator-driven re-seal is required because every daemon
unseals under either key, and any new envelope written during
the overlap is sealed under the new key automatically.

## Preconditions

- `gregale` is on PATH (it's the same binary as `faas`; the
  installer places it at `/usr/local/bin/gregale`).
- Operator has read access to `/etc/faas/secrets/` (root).
- Operator has `journalctl` access (root) to bounce daemons.
- Pre-rotation unseal errors are zero. Check before starting:

  ```sh
  # MFA unseal health (apid)
  psql -U faas -d faas -c \
    "select count(*) from audit_events where event_type = 'mfa.unseal_failed' and created_at > now() - interval '1 hour';"
  # expect: 0

  # Wake secret unseal health (vmmd)
  grep -c 'open age reader' /var/log/faas/vmmd.log | head -1
  # expect: 0 over the last hour

  # Alert dispatch unseal health (meterd)
  grep -c 'alertEvalSkippedDegradedTotal' /var/log/faas/meterd.log | head -1
  # expect: 0 — SkippedNoIdentity counter is the canary
  ```

  If any of these are non-zero, **do not rotate**. Investigate
  the unseal failure first — a rotation amplifies the failure
  across the entire box instead of a single tenant.

- The box is in a quiet window (no in-flight cron invocations,
  no build VM running). Rotation is not destructive, but the
  30-second bounce of four daemons + the 30-day envelope overlap
  is best done off-peak.

## Procedure

Six numbered steps. The expected wall-clock for a clean run is
under 5 minutes plus the four-daemon bounce.

### 1. Generate the new key + pre-flight check

```sh
sudo gregale host-age status
```

Confirm both `host.age` and `host.age.pub` exist with mode
`0400 root:root` and `0444 root:faas` respectively, and no
`host.age.previous` is present (the box is in pre-rotation
state).

```sh
sudo ls -la /etc/faas/secrets/
# expect:
# host.age       0400 root:root  …
# host.age.pub   0444 root:faas  …
# (no host.age.previous)
```

### 2. Rotate (atomically swap)

```sh
sudo gregale host-age rotate --commit
```

This performs three operations in one shell:

1. Reads current `/etc/faas/secrets/host.age`, parses the
   identity.
2. Atomic-rename `host.age` → `host.age.previous` (mode
   preserved).
3. Generates a new identity, writes it to `host.age` with
   mode `0400 root:root`.

The output is the new recipient string:

```
✓ Rotated host.age → host.age.previous; new current written.
  New recipient:                age1qz3p...
  Previous (now .previous):     age1abc...
  Next: chown root:root /etc/faas/secrets/host.age /etc/faas/secrets/host.age.previous && chmod 0400 both
  Next: systemctl restart faas-vmmd first (it owns host.age.pub), then faas-apid faas-meterd faas-githubd
  Next: gregale host-age status (verify all daemons on the new fingerprint after restart)
  Next: 30-day overlap window starts now; run 'gregale host-age prune-previous' after that
```

**The new recipient is what every NEW envelope will be sealed
to.** Record it in your team's secrets-rotation log
(format: `<date> <operator> host.age rotate <recipient-prefix>`)
so the rotation history is auditable.

### 3. Verify the on-disk shape

```sh
sudo ls -la /etc/faas/secrets/
# expect:
# host.age             0400 root:root  (new)
# host.age.previous    0400 root:root  (old)
# host.age.pub         0444 root:faas  (still the OLD recipient — see step 4)
```

```sh
sudo gregale host-age status
```

Output should show two fingerprints (current + previous), their
respective `mtime`, and a 30-day countdown annotation.

### 4. Bounce the daemons — vmmd FIRST, then the rest

The bounce order is load-bearing. **vmmd writes
`/etc/faas/secrets/host.age.pub` from its in-memory identity at
boot** (`pkg/secretbox/hostkey.go:172-181 WriteRecipientFile`); if
apid restarts first it reads the OLD recipient and seals new
envelopes against the OLD key — the daemons won't be able to
unseal them until vmmd comes up and rewrites the file.

```sh
# 1. vmmd FIRST — it picks up host.age, writes the new host.age.pub.
sudo systemctl restart faas-vmmd

# 2. apid SECOND — it reads the new host.age.pub as its sealing key.
sudo systemctl restart faas-apid

# 3. meterd + githubd — unseal-only; they read host.age via LoadHostKeys
#    and don't care which order they bounce in, so long as vmmd and
#    apid are already on the new identity.
sudo systemctl restart faas-meterd faas-githubd
```

daemons don't watch the file — systemd restart re-reads via
`LoadCredential=faas_host_age_identity` (apid) and the
`FAAS_HOST_AGE_IDENTITY_PATH` env var + LoadHostKeys(dir) for the
other three. Without the bounce, daemons still hold the
**pre-rotation** identity and the rotation does nothing.

After vmmd's restart, `host.age.pub` now points at the NEW
recipient:

```sh
sudo ls -la /etc/faas/secrets/
# expect:
# host.age.pub         0444 root:faas  (now the NEW recipient)
# host.age             0400 root:root  (new)
# host.age.previous    0400 root:root  (old)
```

Wait for the daemons to come up clean:

```sh
sudo systemctl status faas-apid faas-vmmd faas-meterd faas-githubd
# expect: active (running) on all four
```

### 5. Verify unseal health post-bounce

```sh
sudo journalctl -u faas-apid --since "5 min ago" | grep -i 'open_failed\|mfa.unseal'
# expect: zero matches

sudo journalctl -u faas-vmmd --since "5 min ago" | grep -i 'open age reader'
# expect: zero matches
```

Then exercise a representative unseal:

```sh
# Pick a customer app that has sealed env vars and confirm
# the read path still works (pre-rotation envelopes should
# unseal via the .previous identity; post-rotation envelopes
# via the current identity).
curl -sS -H "Authorization: Bearer $FAAS_ADMIN_TOKEN" \
    "https://api.gregale.dev/v1/apps/$APP_ID/secrets/$KEY_NAME"
# expect: 200 with the plaintext value
```

### 6. Stamp the rotation in the runbook log

```sh
echo "$(date -Iseconds) <operator> host.age rotate — committed, daemons bounced" \
  | sudo tee -a /var/log/faas/rotation.log
```

## Validation matrix

A rotation is healthy when ALL of the following are true:

| Signal | Source | Healthy value |
|---|---|---|
| `gregale host-age status` shows both fingerprints | operator CLI | current + previous visible |
| All four daemon status lines green | `systemctl is-active` | active (running) |
| `apid_open_failed_total` (Prometheus) | `/metrics` on apid:9090 | 0 |
| `vmmd_unseal_failed_total` | `/metrics` on vmmd:9090 | 0 |
| `alert_evaluator_skipped_total{reason="no_identity"}` | meterd `/metrics` | 0 |
| `githubd_unseal_failed_total` | githubd `/metrics` | 0 |
| Customer MFA confirm path returns 200 | manual curl | yes |
| Customer app-secrets GET returns 200 | manual curl | yes |
| Newly-sealed envelope sealed under new recipient | manual sqlc query | yes (recipient matches current) |

The first row is operator-driven; the next six are telemetry
that the runbook's PostGres query (or `metric-schema` curl) can
verify. The last two are end-to-end probes that exercise both
the pre-rotation and post-rotation unseal paths.

## Rollback

Up until `gregale host-age prune-previous`, the previous key is
still on disk as `host.age.previous`. Rollback is:

```sh
# Stop all four daemons.
sudo systemctl stop faas-vmmd faas-apid faas-meterd faas-githubd

# Restore the previous key as the new current.
sudo mv /etc/faas/secrets/host.age.previous /etc/faas/secrets/host.age
sudo chmod 0400 /etc/faas/secrets/host.age

# Restart in the same order as step 4: vmmd first (it owns
# host.age.pub), then apid (it reads host.age.pub as its sealing
# key), then meterd + githubd.
sudo systemctl start faas-vmmd
sudo systemctl start faas-apid
sudo systemctl start faas-meterd faas-githubd
```

This restores the pre-rotation state exactly: every daemon
loads the single previous identity, every pre-rotation envelope
unseals, every post-rotation envelope (sealed between rotate
and rollback) is permanently unreadable. The cost of a rollback
is the post-rotation envelopes; for a 5-minute rotation window
that's effectively zero. **Do not roll back after
`prune-previous`** — the previous file is gone and the rotation
is irreversible until you re-provision a fresh host.age from
backup.

The `gregale host-age rotate --abort` flag (proposed, not yet
shipped) takes an internal snapshot at rotate-time and restores
from it without the manual steps above. Filed as a follow-up
for v2.

## Escalation

Page `faas-platform-oncall` if **any** of the following holds
for >1 hour after a rotation:

- `alert_evaluator_skipped_total{reason="no_identity"}` > 0
  (canonical canary — the alert evaluator's SkippedNoIdentity
  counter is the single signal that "the unseal side is broken,"
  not the `alert_evaluator_enabled` gauge which only tracks
  "the evaluator is wired.")
- Any customer's MFA confirm endpoint returns 5xx for >1% of
  requests in a 5-minute window.
- A `systemctl status faas-*` line shows `failed` or
  `activating (auto-restart)` for >10 cycles.

Escalation path is documented in
[`docs/ops/escalation.md`](escalation.md) — Tier 0 is a manual
host.age replacement from backup + `prune-previous --force`,
after which the operator must coordinate with every customer
who had MFA enrolled during the rotation window to re-enroll.

## Acceptance

A rotation is considered "complete" when:

1. All entries in the validation matrix are green for 30
   consecutive days after step 4 of the procedure.
2. `gregale host-age prune-previous` runs cleanly (defaults:
   refuses if the previous file is <30 days old).
3. The runbook log (`/var/log/faas/rotation.log`) carries the
   timestamp, operator, and recipient prefix.

Only after step 1 has held for 30 days should the operator run
`gregale host-age prune-previous`. The default 30-day overlap is
the actual security primitive — shortening it requires
operator sign-off and a written justification in
`/var/log/faas/rotation.log`.

## Pruning the previous key (30+ days post-rotation)

```sh
sudo gregale host-age status
# confirm: host.age.previous age = 30+ days
sudo gregale host-age prune-previous
# default: refuses if previous <30 days
# --force: skip the age check
# --promote: rename previous → current instead of removing
```

`--promote` is the manual escape hatch for "the current key was
broken-on-arrival; let me use the previous one as the new
current." It refuses if `host.age` already exists; the operator
must `sudo rm /etc/faas/secrets/host.age` first to use this
escape hatch. The default flow is `prune-previous` (just
delete), which is irreversible.

## References

- `docs/adr/057-host-age-rotation.md` — ADR capturing the v1
  partial-deliverable and v2 follow-up scope.
- `docs/ops/secrets-rotation.md` — the surrounding secrets
  rotation doc; the host.age entry there references this
  runbook.
- `docs/runbooks/multi-host-rollout.md` — Tier 1 Phase 6, which
  requires the rotation runbook to ship before production
  multi-host is enabled.
- `pkg/secretbox/hostkey.go` — the underlying
  `LoadHostKeys(dir)` / `OpenMulti` plumbing.
- `cmd/gregale/commands_host_age.go` — the operator CLI.
