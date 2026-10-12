#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
before=$(balance_micro)
for sender in "$HIGH" "$UNSAFE"; do
  create_invoice
  id=$ID address=$ADDRESS
  transfer=$(make_transfer "$(new_hash)" "$address" "$sender")
  request 201 POST /mock/transfers "$CONTROL" "$transfer"
  advance
  if [[ "$sender" == "$HIGH" ]]; then expected=READY; else expected=HELD; fi
  wait_invoice "$id" ".status == \"OPEN\" and .refund_status == \"$expected\""
  check_json '.refund.amount == "100.000000"'
  refund=$(field '.refund.id'); uuid "$refund"
  get_invoice "$id"
  if [[ "$expected" == READY ]]; then
    check_json '.payment_status == "REJECTED"'
    equal "$(field '.refund.recipient')" "$HIGH" 'Маршрут автоматического возврата'
  else
    check_json '.payment_status == "REJECTED"'
    entries=$(sql 'SELECT count(*) FROM ledger_entries;')
    request 400 POST "/mock/refunds/$refund/approve" "$CONTROL" '{"recipient":"0x0000000000000000000000000000000000000000"}'
    route=$(jq -nc --arg recipient "$SAFE" '{recipient:$recipient}')
    request 200 POST "/mock/refunds/$refund/approve" "$CONTROL" "$route"
    check_json '.status == "READY"'
    first=$BODY
    request 200 POST "/mock/refunds/$refund/approve" "$CONTROL" "$route"
    equal "$BODY" "$first" 'Повтор подтверждения маршрута'
    request 409 POST "/mock/refunds/$refund/approve" "$CONTROL" "$(jq -nc --arg recipient "$LOW" '{recipient:$recipient}')"
    equal "$(sql 'SELECT count(*) FROM ledger_entries;')" "$entries" 'Подтверждение без новых проводок'
    get_invoice "$id"
    check_json '.refund_status == "READY" and .payment_status == "REJECTED"'
  fi
  request 201 POST /mock/transfers "$CONTROL" "$transfer"
  equal "$(sql "SELECT count(*) FROM refunds r JOIN payments p ON p.id=r.payment_id WHERE p.invoice_id='$id';")" 1 'Один возврат'
  equal "$(account_balance "$id" ASSET)" 100000000 'Актив остаётся в системе'
  equal "$(account_balance "$id" REFUND)" 100000000 'Полное обязательство возврата'
  equal "$(account_balance "$id" HOLD)" 0 'Холд закрыт'
  check_delta "$before" 0
  request 409 POST /mock/reorg "$CONTROL" '{"depth":3}'
  pass "HIGH → $expected, полный возврат без комиссии, окончательный депозит защищён"
done
