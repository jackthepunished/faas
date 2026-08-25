#!/usr/bin/env bash
# G19.3 / ADR-127 Layer 12 — five M8 row 5 metal acceptance gates.
#
# The role installs this as an opt-in, non-root oneshot harness. It deliberately
# does not invent app credentials, mutate vmmd, or accept a missing framing
# assertion. Provider-specific API provisioning and rollback commands are
# supplied through the Ansible-rendered environment file.

set -Eeuo pipefail

die() {
  printf 'metal-h2c-acceptance: ERROR: %s\n' "$*" >&2
  exit 1
}

log() {
  printf 'metal-h2c-acceptance: %s\n' "$*"
}

usage() {
  cat >&2 <<'EOF'
usage: metal-acceptance.sh <gate> [fixture...]

gates:
  http1 <id>
  h2c-prior-knowledge <id>
  grpc-trailers <unary-id> <streaming-id>
  surgical-rollback <id>
  wholesale-rollback <id>
  all
EOF
}

load_fixture_env() {
  local env_file="${FAAS_H2C_FIXTURE_ENV:-/etc/faas/metal-h2c-acceptance/fixture-apps.env}"
  [[ -r "$env_file" ]] || die "fixture environment is not readable: $env_file"
  # shellcheck disable=SC1090
  source "$env_file"
}

fixture_key() {
  printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_'
}

fixture_value() {
  local key
  key="$(fixture_key "$1")"
  local field="$2"
  local variable="FAAS_H2C_FIXTURE_${key}_${field}"
  [[ -n "${!variable:-}" ]] || die "fixture $1 has no $field value"
  printf '%s' "${!variable}"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is missing: $1"
}

assert_framing() {
  local fixture_id="$1"
  local expected="$2"
  local command_text="${FAAS_H2C_FRAMING_ASSERT_COMMAND:-}"
  [[ -n "$command_text" ]] || die "FAAS_H2C_FRAMING_ASSERT_COMMAND is required for the metal wire-shape gate"
  FAAS_H2C_FIXTURE_ID="$fixture_id" \
    FAAS_H2C_EXPECTED_FRAMING="$expected" \
    bash -ceu "$command_text"
}

http_probe() {
  local fixture_id="$1"
  local mode="$2"
  local url path headers status timeout
  url="$(fixture_value "$fixture_id" URL)"
  path="$(fixture_value "$fixture_id" PATH)"
  timeout="${FAAS_H2C_ACCEPTANCE_TIMEOUT_SECONDS:-30}"
  headers="$(mktemp)"

  local -a curl_args=(--fail-with-body --silent --show-error --max-time "$timeout" -D "$headers" -o /dev/null)
  if [[ "$mode" == h2c ]]; then
    curl_args+=(--http2-prior-knowledge)
  else
    curl_args+=(--http1.1)
  fi
  if ! curl "${curl_args[@]}" "${url%/}${path}"; then
    rm -f "$headers"
    return 1
  fi
  status="$(awk '$1 ~ /^HTTP\// {code=$2} END {print code}' "$headers")"
  rm -f "$headers"
  [[ "$status" =~ ^2[0-9][0-9]$ ]] || die "$fixture_id returned HTTP status $status"
  assert_framing "$fixture_id" "$(fixture_value "$fixture_id" EXPECTED_FRAMING)"
  log "$fixture_id: $mode response $status"
}

grpc_probe() {
  local fixture_id="$1"
  local target method payload timeout
  target="$(fixture_value "$fixture_id" GRPC_TARGET)"
  method="$(fixture_value "$fixture_id" GRPC_METHOD)"
  payload="$(fixture_value "$fixture_id" GRPC_PAYLOAD)"
  timeout="${FAAS_H2C_ACCEPTANCE_TIMEOUT_SECONDS:-30}"
  require_command grpcurl
  grpcurl -insecure -max-time "$timeout" -d "$payload" "$target" "$method" >/dev/null
  assert_framing "$fixture_id" "$(fixture_value "$fixture_id" EXPECTED_FRAMING)"
  log "$fixture_id: gRPC method $method succeeded"
}

grpc_trailers() {
  local unary_id="$1"
  local streaming_id="$2"
  grpc_probe "$unary_id"
  grpc_probe "$streaming_id"
}

rollback_probe() {
  local fixture_id="$1"
  local switch_command="$2"
  local restore_command="$3"
  [[ -n "$switch_command" ]] || die "rollback switch command is not configured"
  [[ -n "$restore_command" ]] || die "rollback restore command is not configured"

  bash -ceu "$switch_command"
  if ! http_probe "$fixture_id" http1; then
    bash -ceu "$restore_command"
    return 1
  fi
  bash -ceu "$restore_command"
  log "$fixture_id: rollback switch and restoration succeeded"
}

run_gate() {
  local gate="$1"
  shift
  case "$gate" in
    http1)
      [[ $# == 1 ]] || die "http1 expects one fixture ID"
      http_probe "$1" http1
      ;;
    h2c-prior-knowledge)
      [[ $# == 1 ]] || die "h2c-prior-knowledge expects one fixture ID"
      http_probe "$1" h2c
      ;;
    grpc-trailers)
      [[ $# == 2 ]] || die "grpc-trailers expects unary and streaming fixture IDs"
      grpc_trailers "$1" "$2"
      ;;
    surgical-rollback)
      [[ $# == 1 ]] || die "surgical-rollback expects one fixture ID"
      rollback_probe "$1" "${FAAS_H2C_SURGICAL_ROLLBACK_COMMAND:-}" "${FAAS_H2C_SURGICAL_RESTORE_COMMAND:-}"
      ;;
    wholesale-rollback)
      [[ $# == 1 ]] || die "wholesale-rollback expects one fixture ID"
      rollback_probe "$1" "${FAAS_H2C_WHOLESALE_ROLLBACK_COMMAND:-}" "${FAAS_H2C_WHOLESALE_RESTORE_COMMAND:-}"
      ;;
    all)
      [[ $# == 0 ]] || die "all does not accept fixture IDs"
      run_gate http1 app_http1_default
      run_gate h2c-prior-knowledge app_http2_prior_knowledge
      run_gate grpc-trailers app_grpc_unary app_grpc_server_streaming
      run_gate surgical-rollback app_surgical_rollback_target
      run_gate wholesale-rollback app_surgical_rollback_target
      log 'all five G19.3 gates passed'
      ;;
    *)
      usage
      exit 2
      ;;
  esac
}

require_command curl
load_fixture_env
run_gate "${1:-}" "${@:2}"
