#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# Restore provider availability even when an assertion fails.
trap 'curl -sS --max-time 3 -X POST "$BASE_URL/mock/aml" -H "X-Mock-Key: $CONTROL" -H "Content-Type: application/json" -d '\''{"unavailable":false}'\'' >/dev/null || true' EXIT
before=$(balance_micro)
aml true
create_invoice
id=$ID address=$ADDRESS
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$address" "$LOW")"
advance
wait_invoice "$id" '.payment_status == "AML_PENDING" and .status == "OPEN"'
wait_sql "SELECT last_error FROM aml_jobs j JOIN payments p ON p.id=j.payment_id WHERE p.invoice_id='$id';" aml_unavailable
check_delta "$before" 0
equal "$(account_balance "$id" ASSET)" 100000000 'Актив при сбое AML'
equal "$(account_balance "$id" HOLD)" 100000000 'Полный холд при сбое AML'
equal "$(sql "SELECT count(*) FROM aml_results r JOIN payments p ON p.id=r.payment_id WHERE p.invoice_id='$id';")" 0 'AML-решение не выдумано'
equal "$(sql "SELECT count(*) FROM refunds r JOIN payments p ON p.id=r.payment_id WHERE p.invoice_id='$id';")" 0 'Сбой не означает HIGH'
aml false
wait_invoice "$id" '.payment_status == "CREDITED" and .status == "PAID"'
check_delta "$before" 99000000
check_credit "$id"
equal "$(sql "SELECT count(*) FROM aml_results r JOIN payments p ON p.id=r.payment_id WHERE p.invoice_id='$id';")" 1 'Одно AML-решение после восстановления'
pass 'Недоступность AML сохраняет холд; восстановление зачисляет один раз'
