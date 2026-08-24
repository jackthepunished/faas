#!/usr/bin/env bash
# verify-secrets.sh — operator-side smoke test for security review A4.
#
# Asserts that FAAS_SESSION_KEY and the per-host value-hash HMAC key are
# scoped to faas-apid only (loaded via systemd LoadCredential=, NOT via
# EnvironmentFile=/etc/faas/sealed.env which is shared by all six
# control-plane daemons). Run on the EX44
# after `make bootstrap` and a daemon-reload.
#
# Exits 0 if all checks pass; prints each check as ✓/✗ and returns
# non-zero on the first failure. Safe to run repeatedly (read-only).
#
# Usage:
#   sudo deploy/scripts/verify-secrets.sh

set -euo pipefail

pass=0
fail=0

check() {
  local desc="$1"
  shift
  if "$@"; then
    echo "  ✓ ${desc}"
    pass=$((pass+1))
  else
    echo "  ✗ ${desc}"
    fail=$((fail+1))
  fi
}

# 1. The session key file must exist with mode 0400 root:root.
check "/etc/faas/secrets/session.key exists with mode 0400 root:root" bash -c '
  [[ -f /etc/faas/secrets/session.key ]] \
    && [[ "$(stat -c "%a" /etc/faas/secrets/session.key)" == "400" ]] \
    && [[ "$(stat -c "%U:%G" /etc/faas/secrets/session.key)" == "root:root" ]]
'

# 1b. The ADR-117 value-hash key follows the same root-only on-disk
# contract. systemd copies it into apid's private credential directory.
check "/etc/faas/secrets/host.hmac.key exists with mode 0400 root:root" bash -c '
  [[ -f /etc/faas/secrets/host.hmac.key ]] \
    && [[ "$(stat -c "%a" /etc/faas/secrets/host.hmac.key)" == "400" ]] \
    && [[ "$(stat -c "%U:%G" /etc/faas/secrets/host.hmac.key)" == "root:root" ]]
'

# 2. sealed.env MUST NOT carry FAAS_SESSION_KEY any more — that
#    was the A4 leak. Operators migrating from a pre-A4 install need
#    to re-run the v2 secrets init (PR-X `gregale secrets init`, pending)
#    or hand-edit sealed.env to scrub the key. The historical v1
#    bootstrap.sh was retired in issue #911 / PR-1 (ADR-110); the file's
#    existence is asserted first so a fresh host that hasn't bootstrapped
#    at all reports red � rather than silently passing on a missing-file
#    grep (grep -q exits 2 on a missing file, which `!` would otherwise
#    flip to a false-positive 0).
check "sealed.env does NOT contain FAAS_SESSION_KEY" bash -c '
  [[ -f /etc/faas/sealed.env ]] \
    && ! grep -q "^FAAS_SESSION_KEY=" /etc/faas/sealed.env
'

# 3. faas-apid's environment carries FAAS_SESSION_KEY (systemd
#    LoadCredential → Environment= substitution).
check "faas-apid loads FAAS_SESSION_KEY" bash -c '
  systemctl show faas-apid -p Environment 2>/dev/null | grep -q "FAAS_SESSION_KEY"
'

# 3b. The per-host value-hash HMAC key must be scoped to apid through
# the systemd credential directory, never exported through sealed.env.
check "faas-apid loads FAAS_HOST_HMAC_KEY_PATH" bash -c '
  systemctl show faas-apid -p Environment 2>/dev/null | grep -q "FAAS_HOST_HMAC_KEY_PATH"
'

# 4. The other five daemons MUST NOT carry FAAS_SESSION_KEY in
#    their environment — that was the leak surface.
for unit in faas-gatewayd-internal faas-gatewayd-public faas-imaged faas-githubd faas-meterd faas-schedd; do
  check "${unit} does NOT load FAAS_SESSION_KEY" bash -c "
    ! systemctl show ${unit} -p Environment 2>/dev/null | grep -q 'FAAS_SESSION_KEY'
  "
done

# 5. apid's unit file references LoadCredential (defence in depth).
check "faas-apid.service uses LoadCredential=" bash -c '
  grep -q "^LoadCredential=faas_session_key:" /etc/systemd/system/faas-apid.service
'
check "faas-apid.service loads the host HMAC credential" bash -c '
  grep -q "^LoadCredential=faas_host_hmac_key:" /etc/systemd/system/faas-apid.service
'

# 6. PR-P4 + ADR-032 v2 — billing provider mode.
# Paddle is the production billing provider at v2 (ADR-032 v2), so
# FAAS_PADDLE_API_KEY is mandatory on every PRODUCTION-tagged node.
# When sandbox=1 the key MUST start with pdl_sandbox_; otherwise
# it MUST start with pdl_live_. The Stripe legacy opt-in
# (FAAS_BILLING_PROVIDER=stripe) still boots; the Stripe-side
# Paddle-key check is skipped so the legacy path stays reachable
# for the documented node-level rollback.
#
# (The CI static-check at .github/workflows/ci.yml grep -qs for the
# literals 'FAAS_PADDLE_API_KEY' and 'FAAS_BILLING_PROVIDER=paddle'
# inside this file as a regression sentinel — the production default
# is Paddle and the script must name both the key + the provider.)
#
# Dev boxes (Lima / CI runners / local playbooks): this script is
# intended for production-tagged hosts only. The dev-box bill
# is "set FAAS_BILLING_PROVIDER=stripe to skip Paddle (Stripe's
# empty-env path returns nil + name and the apid changePlan 402
# falls through to FAAS_BILLING_PORTAL_URL = '\'''\'')" OR set
# FAAS_PADDLE_API_KEY to any pdl_* value with FAAS_PADDLE_SANDBOX=1
# (the sandbox SDK accepts any key shape; auth fails at runtime,
# not at boot). The CLAUDE.md local loop does not run this script
# against Lima guests.
if [[ -f /etc/faas/sealed.env ]]; then
  if grep -q "^FAAS_BILLING_PROVIDER=stripe" /etc/faas/sealed.env; then
    # Legacy opt-in path. Paddle-api-key check is skipped; the
    # node-level operator has explicit knowledge of the legacy
    # surface and a missing Paddle key is the expected state.
    :
  else
    check "sealed.env has FAAS_PADDLE_API_KEY" bash -c '
      grep -q "^FAAS_PADDLE_API_KEY=pdl_" /etc/faas/sealed.env
    '
    if grep -qE "^FAAS_PADDLE_SANDBOX=(1|true)" /etc/faas/sealed.env; then
      check "FAAS_PADDLE_API_KEY starts with pdl_sandbox_ when sandbox=1" bash -c '
        grep -q "^FAAS_PADDLE_API_KEY=pdl_sandbox_" /etc/faas/sealed.env
      '
    else
      check "FAAS_PADDLE_API_KEY starts with pdl_live_ when sandbox=0" bash -c '
        grep -q "^FAAS_PADDLE_API_KEY=pdl_live_" /etc/faas/sealed.env
      '
    fi
    check "sealed.env has FAAS_PADDLE_WEBHOOK_SECRET" bash -c '
      grep -q "^FAAS_PADDLE_WEBHOOK_SECRET=whk_" /etc/faas/sealed.env
    '
  fi
fi

echo
echo "Summary: ${pass} passed, ${fail} failed"
if [[ "${fail}" -ne 0 ]]; then
  exit 1
fi
