#!/usr/bin/env bash
# Shared assertions. Scenario scripts run only inside run-all.sh's disposable project.
# Globals below are consumed by the scripts that source this library.
# shellcheck disable=SC2034
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
case ${SCENARIO_PROJECT:-} in
  gateway-scenarios-*) ;;
  *) printf 'Запускайте сценарии через scripts/run-all.sh\n' >&2; exit 1 ;;
esac
: "${BASE_URL:?}" "${SCENARIO_TMP:?}"
ALPHA=local-alpha-key
BETA=local-beta-key
CONTROL=local-control-key
LOW=0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
HIGH=0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
UNSAFE=0xdddddddddddddddddddddddddddddddddddddddd
SAFE=0xcccccccccccccccccccccccccccccccccccccccc
WAIT_SECONDS=${WAIT_SECONDS:-30}

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'PASS: %s\n' "$*"; }
equal() { [[ "$1" == "$2" ]] || fail "${3:-значение}: получили '$1', ожидали '$2'"; }
dc() { (cd "$ROOT" && docker compose -f compose.yaml --env-file .env.example -p "$SCENARIO_PROJECT" "$@"); }
refresh_endpoint() { BASE_URL="http://$(dc port gateway 8080)"; export BASE_URL; }
sql() { dc exec -T postgres psql -X -U gateway -d gateway -At -v ON_ERROR_STOP=1 -c "$1" || fail 'SQL-проверка не выполнена'; }

# BODY is overwritten by each request; save IDs before issuing the next request.
request() {
  local expected=$1 method=$2 path=$3 key=${4:-} body=${5:-} idem=${6:-} status response_file
  local args=(-sS --connect-timeout 3 --max-time 10 -X "$method" -H 'Content-Type: application/json')
  args+=(-H "X-API-Key: $key" -H "X-Mock-Key: $key")
  [[ -z "$idem" ]] || args+=(-H "Idempotency-Key: $idem")
  [[ -z "$body" ]] || args+=(--data-binary "$body")
  response_file=$(mktemp "$SCENARIO_TMP/response.XXXXXX")
  status=$(curl "${args[@]}" -o "$response_file" -w '%{http_code}' "$BASE_URL$path") || fail "$method $path: ошибка соединения"
  BODY=$(cat "$response_file")
  rm -f "$response_file"
  [[ "$status" == "$expected" ]] || fail "$method $path: HTTP $status вместо $expected; $BODY"
}
check_json() { jq -e "$1" <<<"$BODY" >/dev/null || fail "JSON не соответствует '$1': $BODY"; }
field() { jq -er "$1" <<<"$BODY" || fail "Нет поля $1: $BODY"; }
uuid() { [[ $1 =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || fail 'Некорректный UUID'; }
new_hash() { printf '0x'; od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
create_invoice() {
  request 201 POST /invoices "$ALPHA" '{"amount":"100","currency":"USDT"}' "scenario-$(new_hash)"
  ID=$(field '.id'); ADDRESS=$(field '.deposit_address'); uuid "$ID"
  check_json '.status == "OPEN" and .amount == "100.000000" and .fee_bps == 100'
}
make_transfer() {
  jq -nc --arg tx "$1" --arg recipient "$2" --arg sender "$3" --arg raw "${4:-100000000}" \
    '{tx_hash:$tx,sender:$sender,recipient:$recipient,raw_amount:$raw}'
}
advance() { request 200 POST /mock/advance "$CONTROL" "{\"blocks\":${1:-2}}"; }
aml() { request 200 POST /mock/aml "$CONTROL" "{\"unavailable\":$1}"; }
get_invoice() { request 200 GET "/invoices/$1" "$ALPHA"; }
wait_invoice() {
  local id=$1 predicate=$2 deadline=$((SECONDS + WAIT_SECONDS))
  while :; do
    get_invoice "$id"
    if jq -e "$predicate" <<<"$BODY" >/dev/null; then return; fi
    (( SECONDS < deadline )) || fail "Таймаут invoice $id: $predicate; $BODY"
    sleep 0.2
  done
}
wait_sql() {
  local query=$1 expected=$2 deadline=$((SECONDS + WAIT_SECONDS)) actual
  while :; do
    actual=$(sql "$query")
    [[ "$actual" != "$expected" ]] || return 0
    (( SECONDS < deadline )) || fail "Таймаут SQL: получили '$actual', ожидали '$expected'"
    sleep 0.2
  done
}
wait_ready() {
  local deadline=$((SECONDS + WAIT_SECONDS))
  until curl -fsS --connect-timeout 1 --max-time 2 "$BASE_URL/readyz" >/dev/null 2>&1; do
    (( SECONDS < deadline )) || fail 'API не стал готовым'
    sleep 0.2
  done
}
balance_micro() {
  request 200 GET /balance "${1:-$ALPHA}"
  check_json '.currency == "USDT" and (.available | test("^[0-9]+\\.[0-9]{6}$"))'
  local value
  value=$(field '.available'); value=${value/./}
  # The runner owns an empty DB; these small test balances fit exactly in Bash integers.
  ((${#value} <= 15)) || fail 'Баланс вне диапазона сценария'
  printf '%s\n' "$((10#$value))"
}
check_delta() { equal "$(balance_micro "${3:-$ALPHA}")" "$(( $1 + $2 ))" 'Изменение доступного баланса'; }
account_balance() {
  uuid "$1"
  sql "SELECT coalesce(sum(CASE WHEN (a.kind='ASSET' AND e.side='DEBIT') OR (a.kind<>'ASSET' AND e.side='CREDIT') THEN e.amount ELSE -e.amount END),0) FROM ledger_accounts a JOIN ledger_entries e ON e.account_id=a.id WHERE a.invoice_id='$1' AND a.kind='$2';"
}
check_credit() {
  uuid "$1"
  equal "$(sql "SELECT count(*) FROM ledger_operations WHERE business_key='payment:'||(SELECT id::text FROM payments WHERE invoice_id='$1')||':credit';")" 1 'Единственное зачисление'
  equal "$(account_balance "$1" HOLD)" 0 'Закрытый холд'
}
check_ledger() {
  equal "$(sql "SELECT count(*) FROM (SELECT o.id FROM ledger_operations o LEFT JOIN ledger_entries e ON e.operation_id=o.id GROUP BY o.id HAVING count(e.id)<2 OR coalesce(sum(CASE WHEN e.side='DEBIT' THEN e.amount ELSE -e.amount END),0)<>0) bad;")" 0 'Балансировка каждой операции'
  equal "$(sql "SELECT count(*) FROM (SELECT a.id,a.kind,coalesce(sum(CASE WHEN (a.kind='ASSET' AND e.side='DEBIT') OR (a.kind<>'ASSET' AND e.side='CREDIT') THEN e.amount ELSE -e.amount END),0) balance FROM ledger_accounts a LEFT JOIN ledger_entries e ON e.account_id=a.id GROUP BY a.id) b WHERE balance<0 OR balance>9223372036854775807;")" 0 'Диапазоны счетов'
}
