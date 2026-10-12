#!/usr/bin/env bash
# Creates and removes only a new, uniquely named Compose project.
set -euo pipefail
export LC_ALL=C
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"
for dependency in bash curl jq docker od; do
  command -v "$dependency" >/dev/null || { printf 'Нужен %s\n' "$dependency" >&2; exit 1; }
done
docker compose version >/dev/null
[[ ${WAIT_SECONDS:-30} =~ ^[1-9][0-9]*$ ]] || { printf 'WAIT_SECONDS должен быть положительным целым\n' >&2; exit 1; }
available=(01-payment.sh 02-refunds.sh 03-aml-outage.sh 04-reorg.sh 05-restart.sh 06-api-validation.sh 07-ignored-transfers.sh 08-balance-limit.sh)
scenarios=("${available[@]}")
if (($#)); then
  scenarios=()
  for name in "$@"; do
    case "$name" in
      01-payment.sh|02-refunds.sh|03-aml-outage.sh|04-reorg.sh|05-restart.sh|06-api-validation.sh|07-ignored-transfers.sh|08-balance-limit.sh) ;;
      *) printf 'Неизвестный сценарий: %s\n' "$name" >&2; exit 1 ;;
    esac
  done
  # Stable order and no duplicates: the balance-limit case deliberately leaves a pending job.
  for candidate in "${available[@]}"; do
    for name in "$@"; do
      if [[ "$candidate" == "$name" ]]; then scenarios+=("$candidate"); break; fi
    done
  done
fi
export SCENARIO_PROJECT
SCENARIO_PROJECT="gateway-scenarios-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
export SCENARIO_TMP
SCENARIO_TMP=$(mktemp -d "${TMPDIR:-/tmp}/gateway-scenarios.XXXXXX")
# Explicit settings override both .env and inherited application settings.
export HTTP_PORT=0 POSTGRES_PORT=0 POSTGRES_PASSWORD=local-demo-password
export CONFIRMATIONS=3 LOCAL_MOCKS=true MOCK_CONTROL_KEY=local-control-key
export PARTNER_ALPHA_KEY=local-alpha-key PARTNER_BETA_KEY=local-beta-key
export AML_UNAVAILABLE=false AML_RETRY_INTERVAL=1s REFUNDS_ENABLED=true
export AML_BLACKLIST=0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb,0xdddddddddddddddddddddddddddddddddddddddd
export REFUND_UNSAFE_SENDERS=0xdddddddddddddddddddddddddddddddddddddddd
dc() { docker compose -f compose.yaml --env-file .env.example -p "$SCENARIO_PROJECT" "$@"; }
cleanup() {
  local code=$?
  trap - EXIT INT TERM
  dc logs --no-color >"$SCENARIO_TMP/containers.log" 2>&1 || true
  if ! dc down --volumes >"$SCENARIO_TMP/cleanup.log" 2>&1; then
    printf 'Не удалось удалить временный project %s: %s/cleanup.log\n' "$SCENARIO_PROJECT" "$SCENARIO_TMP" >&2
    code=1
  fi
  docker image rm "$SCENARIO_PROJECT-gateway" >/dev/null 2>&1 || true
  printf 'Логи: %s\n' "$SCENARIO_TMP"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'Временный Docker project: %s\n' "$SCENARIO_PROJECT"
dc up --build -d >"$SCENARIO_TMP/startup.log" 2>&1 || { cat "$SCENARIO_TMP/startup.log" >&2; exit 1; }
endpoint=$(dc port gateway 8080)
export BASE_URL="http://$endpoint"
# shellcheck source=lib.sh
source "$ROOT/scripts/lib.sh"
wait_ready
equal "$(sql 'SELECT count(*) FROM invoices;')" 0 'Пустая БД'
for scenario in "${scenarios[@]}"; do
  printf '\n=== %s ===\n' "$scenario"
  bash "$ROOT/scripts/$scenario"
  refresh_endpoint
  check_ledger
done
printf '\nСценариев пройдено: %s; операции ledger сбалансированы.\n' "${#scenarios[@]}"
