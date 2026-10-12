#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
before=$(balance_micro)
key="restart-$(new_hash)"
request 201 POST /invoices "$ALPHA" '{"amount":"100"}' "$key"
id=$(field '.id'); address=$(field '.deposit_address'); uuid "$id"
transfer=$(make_transfer "$(new_hash)" "$address" "$LOW")
request 201 POST /mock/transfers "$CONTROL" "$transfer"
advance
wait_invoice "$id" '.status == "PAID" and .payment_status == "CREDITED"'
original=$BODY
create_invoice
refund_id=$ID
request 201 POST /mock/transfers "$CONTROL" "$(make_transfer "$(new_hash)" "$ADDRESS" "$HIGH")"
advance
wait_invoice "$refund_id" '.refund_status == "READY"'
original_refund=$BODY
wait_sql "SELECT count(*) FROM aml_jobs WHERE status IN ('PENDING','RUNNING');" 0
wait_sql 'SELECT scan_hash=head_hash FROM chain_state;' t

# Sorted snapshots of all business tables, not just row counts.
snapshot() {
  local table
  for table in partners invoices invoice_idempotency ledger_accounts ledger_operations ledger_entries chain_state chain_blocks chain_events invoice_transfer_bindings payments payment_observations aml_jobs aml_results refunds; do
    sql "SELECT '$table',coalesce(jsonb_agg(t ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM $table t;"
  done
}
snapshot >"$SCENARIO_TMP/before-restart.txt"
dc restart gateway >/dev/null
old_endpoint=$BASE_URL
refresh_endpoint
if [[ "$BASE_URL" != "$old_endpoint" ]]; then printf 'Docker сменил динамический endpoint: %s → %s\n' "$old_endpoint" "$BASE_URL"; fi
wait_ready
snapshot >"$SCENARIO_TMP/after-restart.txt"
cmp "$SCENARIO_TMP/before-restart.txt" "$SCENARIO_TMP/after-restart.txt" || fail 'Рестарт изменил сохранённые строки'
request 201 POST /invoices "$ALPHA" '{"amount":"100.000000"}' "$key"
equal "$(field '.id')" "$id" 'Идемпотентность после рестарта'
get_invoice "$id"
equal "$(jq -Sc '{id,status,payment_status,payment_id:.payment.id}' <<<"$BODY")" "$(jq -Sc '{id,status,payment_status,payment_id:.payment.id}' <<<"$original")" 'Зачисление после рестарта'
get_invoice "$refund_id"
equal "$(jq -Sc '.refund' <<<"$BODY")" "$(jq -Sc '.refund' <<<"$original_refund")" 'Возврат после рестарта'
request 201 POST /mock/transfers "$CONTROL" "$transfer"
check_delta "$before" 99000000
check_credit "$id"
pass 'Рестарт сохраняет строки 15 таблиц, ключи, зачисление и READY-возврат'
