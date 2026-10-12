#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
before=$(balance_micro)
operations=$(sql 'SELECT count(*) FROM ledger_operations;')
create_invoice
id=$ID address=$ADDRESS
# Valid source events which the scanner must ignore; malformed input is tested separately.
for variant in wrong_token wrong_topic failed_receipt overflow underpayment overpayment zero; do
  transfer=$(make_transfer "$(new_hash)" "$address" "$LOW")
  case "$variant" in
    wrong_token) transfer=$(jq --arg token "$SAFE" '.token_contract=$token' <<<"$transfer") ;;
    wrong_topic) transfer=$(jq --arg topic "$(new_hash)" '.topic0=$topic' <<<"$transfer") ;;
    failed_receipt) transfer=$(jq '.receipt_success=false' <<<"$transfer") ;;
    overflow) transfer=$(jq '.raw_amount="9223372036854775808"' <<<"$transfer") ;;
    underpayment) transfer=$(jq '.raw_amount="99999999"' <<<"$transfer") ;;
    overpayment) transfer=$(jq '.raw_amount="100000001"' <<<"$transfer") ;;
    zero) transfer=$(jq '.raw_amount="0"' <<<"$transfer") ;;
  esac
  request 201 POST /mock/transfers "$CONTROL" "$transfer"
done
advance
wait_sql 'SELECT scan_hash=head_hash FROM chain_state;' t
get_invoice "$id"
check_json '.status == "OPEN" and .payment_status == null and .refund_status == null and (.payment == null)'
equal "$(sql 'SELECT count(*) FROM ledger_operations;')" "$operations" 'Неподходящие события не создают проводки'
check_delta "$before" 0
# Unsupported inputs must not stop the scanner or claim the invoice.
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$address" "$LOW")"
advance
wait_invoice "$id" '.status == "PAID" and .payment_status == "CREDITED"'
check_delta "$before" 99000000
check_credit "$id"
second=$(make_transfer "$(new_hash)" "$address" "$LOW")
request 201 POST /mock/transfers "$CONTROL" "$second"
advance
wait_sql 'SELECT scan_hash=head_hash FROM chain_state;' t
equal "$(sql "SELECT count(*) FROM payments WHERE invoice_id='$id';")" 1 'Первый перевод закреплён'
check_delta "$before" 99000000
check_credit "$id"
pass 'Фильтрация token/topic/receipt/суммы, изоляция переполнения и игнорирование второго перевода'
