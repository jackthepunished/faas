#!/usr/bin/env bash
# verify-secrets.sh — operator-side smoke test for security review A4.
#
# Asserts that FAAS_SESSION_KEY is scoped to faas-apid only (loaded via
# systemd LoadCredential=, NOT via EnvironmentFile=/etc/faas/sealed.env
# which is shared by all six control-plane daemons). Run on the EX44
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

# 2. sealed.env MUST NOT carry FAAS_SESSION_KEY any more — that
#    was the A4 leak. Operators migrating from a pre-A4 bootstrap
#    need to re-run bootstrap.sh to scrub the file. Note: the file's
#    existence is asserted first so a fresh host that hasn't
#    bootstrapped at all reports red ✗ rather than silently passing
#    on a missing-file grep (grep -q exits 2 on a missing file,
#    which `!` would otherwise flip to a false-positive 0).
check "sealed.env does NOT contain FAAS_SESSION_KEY" bash -c '
  [[ -f /etc/faas/sealed.env ]] \
    && ! grep -q "^FAAS_SESSION_KEY=" /etc/faas/sealed.env
'

# 3. faas-apid's environment carries FAAS_SESSION_KEY (systemd
#    LoadCredential → Environment= substitution).
check "faas-apid loads FAAS_SESSION_KEY" bash -c '
  systemctl show faas-apid -p Environment 2>/dev/null | grep -q "FAAS_SESSION_KEY"
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

# 6. PR-P4 — billing provider mode.
# When FAAS_BILLING_PROVIDER=paddle, FAAS_PADDLE_API_KEY MUST be set
# (apid refuses to boot otherwise). When sandbox=1, the key MUST
# start with pdl_sandbox_; otherwise it MUST start with pdl_live_.
# When FAAS_BILLING_PROVIDER is unset or "stripe", this check
# skips (the default path is unchanged).
if [[ -f /etc/faas/sealed.env ]] \
   && grep -q "^FAAS_BILLING_PROVIDER=paddle" /etc/faas/sealed.env; then
  check "sealed.env has FAAS_PADDLE_API_KEY when provider=paddle" bash -c '
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
  check "sealed.env has FAAS_PADDLE_WEBHOOK_SECRET when provider=paddle" bash -c '
    grep -q "^FAAS_PADDLE_WEBHOOK_SECRET=whk_" /etc/faas/sealed.env
  '
fi

echo
echo "Summary: ${pass} passed, ${fail} failed"
if [[ "${fail}" -ne 0 ]]; then
  exit 1
fi
