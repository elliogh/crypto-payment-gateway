-- +goose Up
-- Storage for stages 2 and 3 only. No scanner, simulator or AML runs in stage 1.
CREATE TABLE chain_blocks (
    hash TEXT PRIMARY KEY,
    height BIGINT NOT NULL CHECK (height >= 0),
    parent_hash TEXT REFERENCES chain_blocks(hash),
    timestamp TIMESTAMPTZ NOT NULL,
    canonical BOOLEAN NOT NULL
);
CREATE UNIQUE INDEX canonical_block_height ON chain_blocks(height) WHERE canonical;
CREATE TABLE chain_state (
    chain_id BIGINT PRIMARY KEY CHECK (chain_id = 1),
    head_hash TEXT REFERENCES chain_blocks(hash),
    scan_hash TEXT REFERENCES chain_blocks(hash),
    version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0)
);
CREATE TABLE chain_events (
    block_hash TEXT NOT NULL REFERENCES chain_blocks(hash),
    tx_hash TEXT NOT NULL,
    tx_event_index INTEGER NOT NULL CHECK (tx_event_index >= 0),
    log_index INTEGER NOT NULL CHECK (log_index >= 0),
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
    token_contract TEXT NOT NULL,
    raw_amount NUMERIC(78,0) NOT NULL CHECK (raw_amount BETWEEN 0 AND 115792089237316195423570985008687907853269984665640564039457584007913129639935),
    receipt_success BOOLEAN NOT NULL,
    PRIMARY KEY (block_hash, tx_hash, tx_event_index),
    UNIQUE (block_hash, log_index)
);
CREATE TABLE invoice_transfer_bindings (
    invoice_id UUID PRIMARY KEY REFERENCES invoices(id),
    chain_id BIGINT NOT NULL CHECK (chain_id = 1),
    tx_hash TEXT NOT NULL,
    tx_event_index INTEGER NOT NULL CHECK (tx_event_index >= 0),
    UNIQUE (chain_id, tx_hash, tx_event_index)
);
CREATE TABLE payments (
    id UUID PRIMARY KEY,
    invoice_id UUID NOT NULL UNIQUE REFERENCES invoices(id),
    chain_id BIGINT NOT NULL CHECK (chain_id = 1),
    tx_hash TEXT NOT NULL,
    tx_event_index INTEGER NOT NULL CHECK (tx_event_index >= 0),
    amount BIGINT NOT NULL CHECK (amount > 0),
    sender TEXT NOT NULL,
    observation_version BIGINT NOT NULL CHECK (observation_version > 0),
    status TEXT NOT NULL CHECK (status IN ('CONFIRMING', 'ORPHANED', 'AML_PENDING', 'CREDITED', 'REJECTED', 'HELD')),
    UNIQUE (chain_id, tx_hash, tx_event_index)
);
CREATE TABLE payment_observations (
    payment_id UUID NOT NULL REFERENCES payments(id),
    version BIGINT NOT NULL CHECK (version > 0),
    block_hash TEXT NOT NULL REFERENCES chain_blocks(hash),
    log_index INTEGER NOT NULL CHECK (log_index >= 0),
    canonical BOOLEAN NOT NULL,
    PRIMARY KEY (payment_id, version)
);
CREATE TABLE aml_jobs (
    payment_id UUID NOT NULL,
    observation_version BIGINT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'DONE', 'CANCELLED')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    lease_version BIGINT NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
    last_error TEXT,
    PRIMARY KEY (payment_id, observation_version),
    FOREIGN KEY (payment_id, observation_version) REFERENCES payment_observations(payment_id, version)
);
CREATE INDEX aml_jobs_due ON aml_jobs(next_attempt_at) WHERE status IN ('PENDING', 'RUNNING');
CREATE TABLE aml_results (
    payment_id UUID NOT NULL,
    observation_version BIGINT NOT NULL,
    risk TEXT NOT NULL CHECK (risk IN ('LOW', 'HIGH')),
    checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (payment_id, observation_version),
    FOREIGN KEY (payment_id, observation_version) REFERENCES payment_observations(payment_id, version)
);
CREATE TABLE refunds (
    id UUID PRIMARY KEY,
    payment_id UUID NOT NULL UNIQUE REFERENCES payments(id),
    amount BIGINT NOT NULL CHECK (amount > 0),
    recipient TEXT NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('HELD', 'READY')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE refunds;
DROP TABLE aml_results;
DROP TABLE aml_jobs;
DROP TABLE payment_observations;
DROP TABLE payments;
DROP TABLE invoice_transfer_bindings;
DROP TABLE chain_events;
DROP TABLE chain_state;
DROP TABLE chain_blocks;
