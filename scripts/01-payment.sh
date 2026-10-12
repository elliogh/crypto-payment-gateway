#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
before=$(balance_micro)
create_invoice
id=$ID address=$ADDRESS
tx=$(new_hash)
transfer=$(make_transfer "$tx" "$address" "$LOW")
request 201 POST /mock/transfers "$CONTROL" "$transfer"
inclusion=$BODY
wait_invoice "$id" '.payment_status == "CONFIRMING" and .payment.confirmations == 1'
check_delta "$before" 0
advance 1
wait_invoice "$id" '.payment_status == "CONFIRMING" and .payment.confirmations == 2'
check_delta "$before" 0
advance 1
wait_invoice "$id" '.status == "PAID" and .payment_status == "CREDITED" and .refund_status == null'
payment=$(field '.payment.id')
check_delta "$before" 99000000
check_credit "$id"
equal "$(sql "SELECT sum(e.amount) FROM ledger_entries e JOIN ledger_accounts a ON a.id=e.account_id JOIN ledger_operations o ON o.id=e.operation_id WHERE a.kind='INCOME' AND o.business_key='payment:$payment:credit';")" 1000000 'Комиссия 1 USDT'
request 201 POST /mock/transfers "$CONTROL" "$transfer"
equal "$BODY" "$inclusion" 'Повтор события'
advance 1
wait_sql 'SELECT scan_hash=head_hash FROM chain_state;' t
check_delta "$before" 99000000
check_credit "$id"
request 409 POST /mock/transfers "$CONTROL" "$(jq '.raw_amount="99999999"' <<<"$transfer")"
pass 'N=3, net=99, fee=1, повтор перевода не дублирует зачисление'

# Fee rounding and the second partner: floor(1 microUSDT * 250 / 10000) = 0.
beta_before=$(balance_micro "$BETA")
request 201 POST /invoices "$BETA" '{"amount":"0.000001"}' "tiny-$(new_hash)"
tiny_id=$(field '.id'); tiny_address=$(field '.deposit_address'); uuid "$tiny_id"
check_json '.fee_bps == 250'
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$tiny_address" "$LOW" 1)"
advance
wait_sql "SELECT status FROM payments WHERE invoice_id='$tiny_id';" CREDITED
request 200 GET "/invoices/$tiny_id" "$BETA"
check_json '.status == "PAID"'
check_delta "$beta_before" 1 "$BETA"
check_credit "$tiny_id"
pass 'Второй партнёр и округление комиссии для 1 microUSDT'
