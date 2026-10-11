package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"crypto-payment-gateway/internal/chain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	_ chain.Simulator = (*Store)(nil)
	_ chain.Reader    = (*Store)(nil)
)

// InitializeChain is safe for concurrent startup and preserves an existing chain.
func (s *Store) InitializeChain(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chain initialization: %w", err)
	}
	defer tx.Rollback(context.Background())
	digest := sha256.Sum256([]byte("crypto-payment-gateway/simulator/genesis"))
	genesis := "0x" + hex.EncodeToString(digest[:])
	if _, err = tx.Exec(ctx, `INSERT INTO chain_blocks(hash,height,timestamp,canonical)
		VALUES($1,0,$2,true) ON CONFLICT(hash) DO NOTHING`, genesis, time.Unix(0, 0).UTC()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chain_state(chain_id,head_hash,scan_hash)
		VALUES(1,$1,$1) ON CONFLICT(chain_id) DO NOTHING`, genesis); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Every source/financial mutation locks chain_state first. The shared order is:
// chain_state -> invoices ordered by UUID -> payments ordered by UUID -> ledger accounts.
func (s *Store) beginChainChange(ctx context.Context) (pgx.Tx, chain.Block, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, chain.Block{}, err
	}
	head, err := lockChain(ctx, tx)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, chain.Block{}, err
	}
	return tx, head, nil
}

// Use a fresh statement for head data after waiting for the state lock.
func lockChain(ctx context.Context, tx pgx.Tx) (chain.Block, error) {
	var head chain.Block
	// Read the head in a fresh statement after waiting for the state lock.
	// A joined locking SELECT could retain the previous head's snapshot.
	var headHash string
	err := tx.QueryRow(ctx, `SELECT head_hash FROM chain_state WHERE chain_id=1 FOR UPDATE`).Scan(&headHash)
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT hash,height,coalesce(parent_hash,''),timestamp FROM chain_blocks WHERE hash=$1`, headHash).
			Scan(&head.Hash, &head.Height, &head.ParentHash, &head.Timestamp)
	}
	return head, err
}

func appendBlock(ctx context.Context, tx pgx.Tx, parent chain.Block) (chain.Block, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return chain.Block{}, fmt.Errorf("generate block ID: %w", err)
	}
	digest := sha256.Sum256([]byte(id.String()))
	block := chain.Block{Hash: "0x" + hex.EncodeToString(digest[:]), Height: parent.Height + 1, ParentHash: parent.Hash, Timestamp: time.Now().UTC().Truncate(time.Microsecond)}
	_, err = tx.Exec(ctx, `INSERT INTO chain_blocks(hash,height,parent_hash,timestamp,canonical) VALUES($1,$2,$3,$4,true)`,
		block.Hash, block.Height, block.ParentHash, block.Timestamp)
	return block, err
}

func updateHead(ctx context.Context, tx pgx.Tx, head chain.Block) error {
	_, err := tx.Exec(ctx, `UPDATE chain_state SET head_hash=$1,version=version+1 WHERE chain_id=1`, head.Hash)
	return err
}

const eventColumns = `e.tx_hash,e.tx_event_index,e.log_index,e.sender,e.recipient,e.token_contract,e.topic0,e.raw_amount::text,e.receipt_success`

func scanEvent(row pgx.Row) (chain.Event, error) {
	e := chain.Event{TransferID: chain.TransferID{ChainID: chain.ID}}
	err := row.Scan(&e.TxHash, &e.EventIndex, &e.LogIndex, &e.Sender, &e.Recipient, &e.TokenContract, &e.Topic0, &e.RawAmount, &e.ReceiptSuccess)
	return e, err
}

func previousInclusion(ctx context.Context, tx pgx.Tx, id chain.TransferID) (chain.Event, chain.Inclusion, bool, error) {
	e := chain.Event{TransferID: id}
	i := chain.Inclusion{TransferID: id}
	var canonical bool
	err := tx.QueryRow(ctx, `SELECT `+eventColumns+`,b.hash,b.height,b.canonical
		FROM chain_events e JOIN chain_blocks b ON b.hash=e.block_hash
		WHERE e.tx_hash=$1 AND e.tx_event_index=$2 ORDER BY b.canonical DESC,b.timestamp DESC,b.hash LIMIT 1`, id.TxHash, id.EventIndex).
		Scan(&e.TxHash, &e.EventIndex, &e.LogIndex, &e.Sender, &e.Recipient, &e.TokenContract, &e.Topic0, &e.RawAmount, &e.ReceiptSuccess, &i.BlockHash, &i.BlockHeight, &canonical)
	i.LogIndex = e.LogIndex
	return e, i, canonical, err
}

func sameTransfer(a, b chain.Event) bool {
	return a.TransferID == b.TransferID && a.Sender == b.Sender && a.Recipient == b.Recipient &&
		a.TokenContract == b.TokenContract && a.Topic0 == b.Topic0 && a.RawAmount == b.RawAmount && a.ReceiptSuccess == b.ReceiptSuccess
}

func insertEvent(ctx context.Context, tx pgx.Tx, block chain.Block, e chain.Event) (chain.Inclusion, error) {
	_, err := tx.Exec(ctx, `INSERT INTO chain_events(block_hash,tx_hash,tx_event_index,log_index,sender,recipient,token_contract,topic0,raw_amount,receipt_success)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10)`, block.Hash, e.TxHash, e.EventIndex, e.LogIndex, e.Sender, e.Recipient, e.TokenContract, e.Topic0, e.RawAmount, e.ReceiptSuccess)
	return chain.Inclusion{TransferID: e.TransferID, BlockHash: block.Hash, BlockHeight: block.Height, LogIndex: e.LogIndex}, err
}

func (s *Store) AddTransfer(ctx context.Context, event chain.Event) (chain.Inclusion, error) {
	e, err := event.Normalize()
	if err != nil {
		return chain.Inclusion{}, err
	}
	tx, head, err := s.beginChainChange(ctx)
	if err != nil {
		return chain.Inclusion{}, fmt.Errorf("lock simulator chain: %w", err)
	}
	defer tx.Rollback(context.Background())
	old, inclusion, canonical, err := previousInclusion(ctx, tx, e.TransferID)
	if err == nil {
		if !sameTransfer(old, e) {
			return chain.Inclusion{}, chain.ErrConflict
		}
		if canonical {
			return inclusion, tx.Commit(ctx)
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return chain.Inclusion{}, err
	}
	block, err := appendBlock(ctx, tx, head)
	if err != nil {
		return chain.Inclusion{}, err
	}
	inclusion, err = insertEvent(ctx, tx, block, e)
	if err != nil {
		return chain.Inclusion{}, err
	}
	if err = updateHead(ctx, tx, block); err != nil {
		return chain.Inclusion{}, err
	}
	return inclusion, tx.Commit(ctx)
}

func (s *Store) Advance(ctx context.Context, count int) (chain.Block, error) {
	if count < 1 || count > chain.MaxBlocks {
		return chain.Block{}, chain.ErrInvalid
	}
	tx, head, err := s.beginChainChange(ctx)
	if err != nil {
		return chain.Block{}, err
	}
	defer tx.Rollback(context.Background())
	for range count {
		head, err = appendBlock(ctx, tx, head)
		if err != nil {
			return chain.Block{}, err
		}
	}
	if err = updateHead(ctx, tx, head); err != nil {
		return chain.Block{}, err
	}
	return head, tx.Commit(ctx)
}

// rollbackChain changes source data only; the caller owns the chain lock/transaction.
func rollbackChain(ctx context.Context, tx pgx.Tx, ancestor chain.Block) error {
	if _, err := tx.Exec(ctx, `UPDATE chain_blocks SET canonical=false WHERE canonical AND height>$1`, ancestor.Height); err != nil {
		return err
	}
	// If the cursor lies in the removed suffix, rewind it in the same transaction.
	if _, err := tx.Exec(ctx, `UPDATE chain_state SET scan_hash=$1 WHERE chain_id=1 AND scan_hash IN
		(SELECT hash FROM chain_blocks WHERE height>$2)`, ancestor.Hash, ancestor.Height); err != nil {
		return err
	}
	if err := updateHead(ctx, tx, ancestor); err != nil {
		return err
	}
	return nil
}

func (s *Store) ReadBatch(ctx context.Context) (chain.Batch, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return chain.Batch{}, err
	}
	defer tx.Rollback(context.Background())
	var batch chain.Batch
	var height int64
	err = tx.QueryRow(ctx, `SELECT c.version,c.scan_hash,b.height FROM chain_state c
		JOIN chain_blocks b ON b.hash=c.scan_hash WHERE c.chain_id=1`).Scan(&batch.Version, &batch.Cursor, &height)
	if err != nil {
		return chain.Batch{}, err
	}
	rows, err := tx.Query(ctx, `SELECT hash,height,coalesce(parent_hash,''),timestamp FROM chain_blocks
		WHERE canonical AND height>$1 ORDER BY height LIMIT $2`, height, chain.MaxBlocks)
	if err != nil {
		return chain.Batch{}, err
	}
	for rows.Next() {
		var b chain.Block
		if err = rows.Scan(&b.Hash, &b.Height, &b.ParentHash, &b.Timestamp); err != nil {
			break
		}
		batch.Blocks = append(batch.Blocks, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return chain.Batch{}, err
	}
	for i := range batch.Blocks {
		rows, err = tx.Query(ctx, `SELECT `+eventColumns+` FROM chain_events e WHERE e.block_hash=$1 ORDER BY e.log_index`, batch.Blocks[i].Hash)
		if err != nil {
			return chain.Batch{}, err
		}
		for rows.Next() {
			var e chain.Event
			e, err = scanEvent(rows)
			if err != nil {
				break
			}
			batch.Blocks[i].Events = append(batch.Blocks[i].Events, e)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return chain.Batch{}, err
		}
	}
	return batch, tx.Commit(ctx)
}
