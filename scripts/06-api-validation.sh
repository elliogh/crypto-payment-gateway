#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
before=$(balance_micro)
operations=$(sql 'SELECT count(*) FROM ledger_operations;')
key="concurrent-$(new_hash)"
# Each subshell has its own response file. Wait for every child, including failures.
pids=()
for n in 1 2 3 4 5 6 7 8; do
  (
    request 201 POST /invoices "$ALPHA" '{"amount":"100"}' "$key"
    field '.id' >"$SCENARIO_TMP/concurrent-$n.txt"
  ) &
  pids+=("$!")
done
failed=0
for pid in "${pids[@]}"; do wait "$pid" || failed=1; done
equal "$failed" 0 'Конкурентные запросы успешны'
id=$(cat "$SCENARIO_TMP/concurrent-1.txt"); uuid "$id"
for n in 2 3 4 5 6 7 8; do equal "$(cat "$SCENARIO_TMP/concurrent-$n.txt")" "$id" 'Один ID при конкурентном создании'; done
equal "$(sql "SELECT count(*) FROM invoice_idempotency WHERE key='$key';")" 1 'Одна запись идемпотентности'
request 201 POST /invoices "$ALPHA" '{"amount":"100.000000"}' "$key"
equal "$(field '.id')" "$id" 'Эквивалентная сумма'
request 409 POST /invoices "$ALPHA" '{"amount":"101"}' "$key"
check_json '.error.code == "idempotency_conflict"'
request 201 POST /invoices "$BETA" '{"amount":"100"}' "$key"
[[ "$(field '.id')" != "$id" ]] || fail 'Ключи разных партнёров смешаны'
request 404 GET "/invoices/$id" "$BETA"
request 401 GET "/invoices/$id"
request 401 GET /balance "$CONTROL"
request 401 POST /mock/advance "$ALPHA" '{"blocks":1}'
request 404 GET /invoices/not-a-uuid "$ALPHA"
upper=$(printf '%s' "$id" | tr 'a-f' 'A-F')
request 200 GET "/invoices/$upper" "$ALPHA"
equal "$(field '.id')" "$id" 'UUID без учёта регистра'
for body in '{"amount":"0"}' '{"amount":"-1"}' '{"amount":"0.0000001"}' '{"amount":"9223372036854.775808"}' '{"amount":100}' '{"amount":"1","currency":"ETH"}' '{"amount":"1","unknown":true}' '{"amount":"1"} {}' 'null'; do
  request 400 POST /invoices "$ALPHA" "$body" "invalid-$(new_hash)"
  check_json '.error.code | type == "string"'
done
request 400 POST /invoices "$ALPHA" '{"amount":"1"}'
request 400 POST /invoices "$ALPHA" '{"amount":"1"}' 'contains space'
request 413 POST /invoices "$ALPHA" "$(printf '%4097s' ' ')" "large-$(new_hash)"
for body in '{"blocks":0}' '{"blocks":101}' '{"blocks":1,"unknown":true}' '{"blocks":1} {}' 'null'; do
  request 400 POST /mock/advance "$CONTROL" "$body"
done
request 400 POST /mock/aml "$CONTROL" '{}'
request 400 POST /mock/aml "$CONTROL" '{"unavailable":"true"}'
request 400 POST /mock/reorg "$CONTROL" '{"depth":0}'
request 400 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$LOW" "$LOW" 115792089237316195423570985008687907853269984665640564039457584007913129639936)"
request 404 POST /mock/refunds/00000000-0000-0000-0000-000000000000/approve "$CONTROL" "$(jq -nc --arg recipient "$SAFE" '{recipient:$recipient}')"
check_delta "$before" 0
equal "$(sql 'SELECT count(*) FROM ledger_operations;')" "$operations" 'Ошибочные запросы не проводят деньги'
pass 'Конкурентная идемпотентность, изоляция, авторизация, JSON, UUID, размер тела и денежные границы'
