-- +goose Up
-- Existing rows describe decoded Transfer events; new simulator input can also
-- contain other signatures so the scanner must validate them explicitly.
ALTER TABLE chain_events ADD COLUMN topic0 TEXT NOT NULL
    DEFAULT '0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef';
ALTER TABLE chain_events ALTER COLUMN topic0 DROP DEFAULT;
CREATE INDEX chain_events_transfer ON chain_events(tx_hash,tx_event_index);

-- +goose Down
DROP INDEX chain_events_transfer;
ALTER TABLE chain_events DROP COLUMN topic0;
