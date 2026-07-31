# ADR-057: host.age rotation (v1 partial-deliverable)

## Status
Accepted (issue #316). v2 re-seal follow-up filed as
`issue-316-followup-rekey`.

## Context

The X25519 host identity at `/etc/faas/secrets/host.age`
(mode `0400 root:root`, spec §11) seals every customer-visible
secret on the box:

- per-app `app_secrets` envelopes (apid writes at PUT, reads at
  GET)
- per-instance TOTP MFA secrets (apid reads via
  `LoadCredential=faas_host_age_identity` during /verify +
  /confirm + /recover + /disable)
- per-wake secret env vars (vmmd reads + injects into the
  jailer chroot at wake)
- githubd install tokens (sealed at OAuth mint, unsealed at
  cold-start rehydrate)
- alert evaluator webhook secrets (meterd unseals to dispatch
  PromQL-fired alerts)

Before ADR-057, there was no rotation procedure documented and
no plumbing to keep the box unsealing envelopes sealed under
the **previous** key. The on-disk file was effectively
write-once per box lifetime.

Three conditions made this acceptable-for-now but a
Tier-1-blocker for production multi-host:

1. The bootstrap sequence on the EX44 generates the key
   exactly once (vmmd's first-boot path), and the file lived at
   a stable path forever. Replacing it was an undocumented
   manual operation that would have bricked every customer's
   MFA enrollment.
2. The multi-host rollout (`docs/runbooks/multi-host-rollout.md`)
   ships a per-host egress policy, an OCI registry, an mTLS
   handshake, and a `node_signature` — but each host's
   `host.age` is local to that host, and a key compromise on
   host A does not help an attacker read host B's envelopes
   (each host has its own identity). What host.age rotation
   buys is *forward secrecy* on a single host, which the
   multi-host rollout explicitly does NOT cover.
3. There was one latent bug in the production code path
   (`cmd/githubd/main.go:223` defaulted to
   `/etc/faas/secrets/host.age.key` with a stray `.key`
   suffix that didn't match any other component's path) and
   one stale comment (`pkg/auth/hash.go:34-37,86-89` claiming
   the audit-HMAC key was derived from `host.age` via HKDF
   when it actually wasn't). Both are fixed in this PR.

## Decision

ADR-057 ships the **v1 partial-deliverable**:

1. **Two-step rotation** — `rotate` (new key + atomically
   move old to `host.age.previous`) + 30-day overlap (both
   files decrypted at unseal time) + `prune-previous` (remove
   old, refuse to unseal pre-rotation envelopes).

2. **age's native multi-recipient support** —
   `Encrypt(dst, recipients ...)` and
   `Decrypt(src, identities ...)` are variadic; "all identities
   tried until one decrypts" gives the overlap window without
   schema migration.

3. **Operator CLI** — `gregale host-age {init,rotate,status,prune-previous}`,
   mirroring the `sign-keys` shape. Mode 0400 on every file
   the CLI writes; no logging of secret material.

4. **systemd `LoadCredential=` second slot** — three copies
   of `faas-apid.service` (production + control-plane +
   ansible-provisioned) gain a second entry copying
   `host.age.previous` into the unit's credential dir as a
   sibling of the current credential. systemd materialises
   whatever's on disk; the LoadCredential is a no-op
   pre-rotation.

5. **Audit-HMAC stays independent of host.age** — the actual
   code path (`cmd/apid/main.go:706-780`) is independently
   generated random 32-byte key persisted to
   `/var/lib/faas/audit-hmac.key` mode 0o600. The stale
   `pkg/auth/hash.go:34-37,86-89` comments claiming "loaded
   from host.age" + "HKDF over the identity" get corrected.
   `events.data.email_hash` values are stable across
   host.age rotation.

6. **No re-seal in v1** — envelopes sealed under the previous
   key remain sealed under the previous key; only the
   unseal side learns the new identity via the 30-day
   overlap. Background re-seal of pre-rotation envelopes is a
   v2 follow-up.

## Consequences

### Positive

- The host identity is now rotatable under documented
  procedure. Issue #316 / this ADR is the last literal
  blocker on the multi-host rollout's
  "production-ready" criterion.
- The 30-day overlap window gives every customer envelope a
  30-day grace period to be naturally re-sealed (any PUT
  during the overlap seals under the new key). For an
  operator who only touches `app_secrets` once per
  deployment, the entire box transitions to the new key
  within the first refresh after rotate.
- The `gregale host-age status` command gives operators a
  single view of "current fingerprint, previous fingerprint,
  overlap age, daemons on current vs stale fingerprint"
  without inspecting logs.

### Negative

- **No forward secrecy on envelopes sealed before rotate.**
  An attacker who captures a ciphertext AND the previous
  host.age can decrypt indefinitely. The 30-day overlap
  window exists precisely so the operator can drive new
  PUTs to overwrite old ciphertexts, but a customer who
  never updates a sealed env var keeps their envelope
  readable by an attacker holding the previous key for the
  lifetime of that envelope.
- **Bounce window.** The four-daemon bounce during
  rotation (`apid`, `vmmd`, `meterd`, `githubd`) is
  ~30 seconds of unseal-failure. For the alert evaluator
  this means PromQL-fired alerts drop on the floor for the
  bounce duration — `alertEvalSkippedDegradedTotal`
  increments. The Prometheus 30-second scrape interval is
  the canary.
- **Latent bug surface area.** The
  `cmd/githubd/main.go:223` path-bug was a real latent bug
  that would have manifested on the first host.age
  rotation. Reconciled here. Any other component still
  reading `/etc/faas/secrets/host.age.key` would silently
  fail unseal post-rotation; the only such component was
  githubd.

### Compatibility

- `LoadHostKey(path)` is preserved unchanged for backward
  compat with single-identity callers. New callers
  (every daemon in this PR) use `LoadHostKeys(dir)` which
  returns a 1-element slice in the pre-rotation state —
  byte-identical behavior.
- `Open(identity, blob)` is preserved unchanged as a
  1-element wrapper around `OpenMulti`. Same for
  `OpenBytes`. New unseal sites pass the slice directly.
- The githubd `Identity *age.X25519Identity` field is
  preserved alongside the new `Identities []*age.X25519Identity`
  field. Call sites prefer `Identities`, fall back to
  `Identity` in a 1-element slice. This keeps every existing
  test passing without modification.

## v2 follow-up (issue-316-followup-rekey)

The v2 follow-up adds background re-seal:

1. A `pkg/rekey` package that walks every `app_secrets` row
   sealed to the previous key and re-seals under the current
   key.
2. A config knob `FAAS_REKEY_ENABLED=true|false` so operators
   can opt out for compliance reasons (some compliance
   frameworks want the historical recipient preserved on
   every envelope for audit; re-seal would destroy that).
3. A status endpoint `GET /v1/admin/secrets/rekey-progress`
   that surfaces the rekey progress.
4. A background daemon (or cron entry) that drives the
   re-seal at off-peak hours.

The v1 deliverable is sufficient for the solo-operator
deployment because every daemon unseals under either key and
any new envelope written during the overlap seals under the
new key. The v2 follow-up is the "every historical envelope
gets the new recipient" property, which is a defense-in-depth
concern rather than a security-critical one for a single-box
deployment.

## References

- `docs/ops/host-age-rotation.md` — the operator runbook.
- `pkg/secretbox/hostkey.go` — `LoadHostKeys`,
  `WriteHostKeyAtPath`, `PromotePreviousToCurrent`.
- `pkg/secretbox/seal.go` — `OpenMulti`, `OpenBytesMulti`.
- `cmd/gregale/commands_host_age.go` — operator CLI.
- `docs/runbooks/multi-host-rollout.md` — the rollout this
  ADR unblocks.
