#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# Use SQL NUMERIC, not jq floating point or Bash arithmetic, at the int64 boundary.
sql "UPDATE partners SET fee_bps=0 WHERE name='beta';" >/dev/null
raw=$(sql "SELECT (9223372036854775807::numeric-coalesce(sum(CASE WHEN e.side='CREDIT' THEN e.amount ELSE -e.amount END),0))::text FROM ledger_accounts a JOIN partners p ON p.id=a.partner_id LEFT JOIN ledger_entries e ON e.account_id=a.id WHERE a.kind='PARTNER_AVAILABLE' AND p.name='beta';")
[[ "$raw" =~ ^[0-9]+$ ]] || fail 'Неверная граничная сумма'
amount=$(sql "SELECT trunc($raw::numeric/1000000)::text||'.'||lpad(mod($raw::numeric,1000000)::text,6,'0');")
request 201 POST /invoices "$BETA" "$(jq -nc --arg amount "$amount" '{amount:$amount}')" "limit-$(new_hash)"
max_id=$(field '.id'); address=$(field '.deposit_address'); uuid "$max_id"
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$address" "$LOW" "$raw")"
advance
wait_sql "SELECT status FROM payments WHERE invoice_id='$max_id';" CREDITED
request 200 GET /balance "$BETA"
equal "$(field '.available')" 9223372036854.775807 'Баланс MaxInt64 без потери точности'
check_credit "$max_id"
request 201 POST /invoices "$BETA" '{"amount":"100"}' "overflow-balance-$(new_hash)"
held_id=$(field '.id'); address=$(field '.deposit_address'); uuid "$held_id"
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$address" "$LOW")"
advance
wait_sql "SELECT last_error FROM aml_jobs j JOIN payments p ON p.id=j.payment_id WHERE p.invoice_id='$held_id';" balance_limit
request 200 GET "/invoices/$held_id" "$BETA"
check_json '.payment_status == "AML_PENDING" and .status == "OPEN"'
equal "$(account_balance "$held_id" HOLD)" 100000000 'Переполнение сохраняет холд'
equal "$(sql "SELECT count(*) FROM aml_results r JOIN payments p ON p.id=r.payment_id WHERE p.invoice_id='$held_id';")" 0 'Решение не фиксируется частично'
request 200 GET /balance "$BETA"
equal "$(field '.available')" 9223372036854.775807 'Переполнение не меняет баланс'
before=$(balance_micro)
create_invoice
id=$ID
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$ADDRESS" "$LOW")"
advance
wait_invoice "$id" '.payment_status == "CREDITED" and .status == "PAID"'
check_delta "$before" 99000000
check_credit "$id"
pass 'Точный MaxInt64; превышение баланса сохраняет холд и не останавливает другого партнёра'
