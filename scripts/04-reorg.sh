#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
trap 'curl -sS --max-time 3 -X POST "$BASE_URL/mock/aml" -H "X-Mock-Key: $CONTROL" -H "Content-Type: application/json" -d '\''{"unavailable":false}'\'' >/dev/null || true' EXIT
before=$(balance_micro)
create_invoice
id=$ID address=$ADDRESS
transfer=$(make_transfer "$(new_hash)" "$address" "$LOW")
request 201 POST /mock/transfers "$CONTROL" "$transfer"
wait_invoice "$id" '.payment_status == "CONFIRMING" and .payment.confirmations == 1'
payment=$(field '.payment.id'); uuid "$payment"
request 200 POST /mock/reorg "$CONTROL" '{"depth":1}'
wait_invoice "$id" '.payment_status == "ORPHANED" and .payment.confirmations == 0 and .payment.observation_version == 2'
equal "$(field '.payment.id')" "$payment" 'Payment ID после реорга до N'
equal "$(account_balance "$id" ASSET)" 0 'До N нет финансовых проводок'
aml true
transfer=$(jq '.log_index=7' <<<"$transfer")
request 201 POST /mock/transfers "$CONTROL" "$transfer"
advance
wait_invoice "$id" '.payment_status == "AML_PENDING" and .payment.observation_version == 3'
wait_sql "SELECT last_error FROM aml_jobs WHERE payment_id='$payment' AND observation_version=3;" aml_unavailable
equal "$(account_balance "$id" HOLD)" 100000000 'Холд нового наблюдения'
request 200 POST /mock/reorg "$CONTROL" '{"depth":3}'
wait_invoice "$id" '.payment_status == "ORPHANED" and .payment.observation_version == 4'
equal "$(account_balance "$id" ASSET)" 0 'Реорг отменяет актив'
equal "$(account_balance "$id" HOLD)" 0 'Реорг отменяет холд'
equal "$(sql "SELECT status FROM aml_jobs WHERE payment_id='$payment' AND observation_version=3;")" CANCELLED 'Старая задача отменена'
check_delta "$before" 0
transfer=$(jq '.log_index=9' <<<"$transfer")
request 201 POST /mock/transfers "$CONTROL" "$transfer"
aml false
advance
wait_invoice "$id" '.payment_status == "CREDITED" and .status == "PAID" and .payment.observation_version == 5'
equal "$(field '.payment.id')" "$payment" 'Повторное включение сохраняет Payment ID'
equal "$(sql "SELECT string_agg(observation_version::text,',') FROM aml_results WHERE payment_id='$payment';")" 5 'Только актуальное AML-решение'
check_delta "$before" 99000000
check_credit "$id"
head=$(sql 'SELECT head_hash FROM chain_state;')
request 409 POST /mock/reorg "$CONTROL" '{"depth":3}'
equal "$(sql 'SELECT head_hash FROM chain_state;')" "$head" 'Отклонённый реорг не меняет цепь'
pass 'Реорг до N и с холдом, отмена старой задачи, reinclusion с новым log_index, защита зачисления'
