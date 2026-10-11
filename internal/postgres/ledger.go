package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"crypto-payment-gateway/internal/ledger"
	"crypto-payment-gateway/internal/money"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrLedgerIsolation = errors.New("ledger posting requires READ COMMITTED isolation")

func (s *Store) PostLedger(ctx context.Context, key string, entries []ledger.Entry) (string, error) {
	tx, err := s.BeginLedgerTx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())

	id, err := s.PostLedgerTx(ctx, tx, key, entries)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit ledger posting: %w", err)
	}
	return id, nil
}

// PostLedgerTx lets financial status changes share the posting's ACID transaction.
// It accepts only a root LedgerTx created by BeginLedgerTx, never a pgx savepoint.
// On any error it rolls back the whole tx, including changes before this call.
// On success the caller still owns Commit. Existing operations are only replayed;
// this module never updates journal rows or appends to an existing operation.
// READ COMMITTED ensures the balance read sees postings committed while waiting
// for account locks; a repeatable snapshot could otherwise allow overspending.
func (s *Store) PostLedgerTx(ctx context.Context, tx *LedgerTx, key string, entries []ledger.Entry) (string, error) {
	id, err := s.postLedgerTx(ctx, tx.tx, key, entries)
	if err == nil {
		return id, nil
	}
	rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rollbackErr := tx.Rollback(rollback); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
		err = errors.Join(err, fmt.Errorf("rollback ledger posting: %w", rollbackErr))
	}
	return "", err
}

func (s *Store) postLedgerTx(ctx context.Context, tx pgx.Tx, key string, entries []ledger.Entry) (string, error) {
	if len(key) == 0 || len(key) > 200 {
		return "", ledger.ErrEntry
	}
	fingerprint, err := ledger.Fingerprint(entries)
	if err != nil {
		return "", err
	}
	var isolation string
	if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return "", fmt.Errorf("read ledger transaction isolation: %w", err)
	}
	if isolation != string(pgx.ReadCommitted) {
		return "", ErrLedgerIsolation
	}
	operationID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate ledger operation ID: %w", err)
	}
	id := operationID.String()
	const reserveQuery = `
        INSERT INTO ledger_operations (id, business_key, fingerprint)
        VALUES ($1, $2, $3)
        ON CONFLICT (business_key) DO NOTHING
        RETURNING id::text`
	var insertedID string
	err = tx.QueryRow(ctx, reserveQuery, id, key, fingerprint).Scan(&insertedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return replayLedgerOperation(ctx, tx, key, fingerprint)
	}
	if err != nil {
		return "", fmt.Errorf("reserve ledger operation: %w", err)
	}
	accountKinds, err := lockLedgerAccounts(ctx, tx, entries)
	if err != nil {
		return "", err
	}
	if err := checkLedgerBalances(ctx, tx, accountKinds, entries); err != nil {
		return "", err
	}

	const entryQuery = `
        INSERT INTO ledger_entries (operation_id, account_id, side, amount)
        VALUES ($1, $2, $3, $4::bigint)`
	for _, entry := range entries {
		if _, err := tx.Exec(ctx, entryQuery, id, entry.AccountID, string(entry.Side), int64(entry.Amount)); err != nil {
			return "", fmt.Errorf("insert ledger entry: %w", err)
		}
	}
	return id, nil
}

func replayLedgerOperation(ctx context.Context, tx pgx.Tx, key, fingerprint string) (string, error) {
	const query = `
        SELECT id::text, fingerprint
        FROM ledger_operations
        WHERE business_key = $1`
	var id, storedFingerprint string
	if err := tx.QueryRow(ctx, query, key).Scan(&id, &storedFingerprint); err != nil {
		return "", fmt.Errorf("read ledger operation: %w", err)
	}
	if storedFingerprint != fingerprint {
		return "", ledger.ErrConflict
	}
	return id, nil
}

func lockLedgerAccounts(ctx context.Context, tx pgx.Tx, entries []ledger.Entry) (map[string]ledger.AccountKind, error) {
	unique := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		unique[entry.AccountID] = struct{}{}
	}
	accountIDs := make([]string, 0, len(unique))
	for id := range unique {
		accountIDs = append(accountIDs, id)
	}
	sort.Strings(accountIDs)

	// Every posting takes account locks in UUID order to avoid deadlocks.
	const query = `
        SELECT id::text, kind
        FROM ledger_accounts
        WHERE id = ANY($1::uuid[]) AND currency = 'USDT'
        ORDER BY id
        FOR UPDATE`
	rows, err := tx.Query(ctx, query, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("lock ledger accounts: %w", err)
	}
	defer rows.Close()
	kinds := make(map[string]ledger.AccountKind, len(accountIDs))
	for rows.Next() {
		var id string
		var kind ledger.AccountKind
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, fmt.Errorf("scan locked ledger account: %w", err)
		}
		kinds[id] = kind
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locked ledger accounts: %w", err)
	}
	if len(kinds) != len(accountIDs) {
		return nil, ledger.ErrEntry
	}
	return kinds, nil
}

func checkLedgerBalances(ctx context.Context, tx pgx.Tx, kinds map[string]ledger.AccountKind, entries []ledger.Entry) error {
	type totals struct{ debit, credit money.USDT }
	changes := make(map[string]totals, len(kinds))
	for _, entry := range entries {
		total := changes[entry.AccountID]
		// Fingerprint has already checked the operation's debit and credit totals,
		// so each account's nonnegative subtotal also fits int64.
		if entry.Side == ledger.Debit {
			total.debit += entry.Amount
		} else {
			total.credit += entry.Amount
		}
		changes[entry.AccountID] = total
	}
	accountIDs := make([]string, 0, len(kinds))
	for id := range kinds {
		accountIDs = append(accountIDs, id)
	}

	// This must be a separate statement AFTER all account locks are acquired:
	// READ COMMITTED gives it a fresh snapshot including the preceding writer.
	// Sum the history once per touched account, not once per inserted entry.
	const query = `
		SELECT a.id::text,
			COALESCE(sum(CASE
				WHEN (a.kind = 'ASSET' AND e.side = 'DEBIT')
					OR (a.kind <> 'ASSET' AND e.side = 'CREDIT')
				THEN e.amount ELSE -e.amount END), 0)::text
		FROM ledger_accounts a
		LEFT JOIN ledger_entries e ON e.account_id = a.id
		WHERE a.id = ANY($1::uuid[])
		GROUP BY a.id`
	rows, err := tx.Query(ctx, query, accountIDs)
	if err != nil {
		return fmt.Errorf("read ledger account balances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, rawBalance string
		if err := rows.Scan(&id, &rawBalance); err != nil {
			return fmt.Errorf("scan ledger account balance: %w", err)
		}
		balance, err := strconv.ParseInt(rawBalance, 10, 64)
		if err != nil || balance < 0 {
			return ledger.ErrBalance
		}
		total := changes[id]
		if _, err := ledger.NextBalance(kinds[id], money.USDT(balance), total.debit, total.credit); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read ledger account balances: %w", err)
	}
	return nil
}
