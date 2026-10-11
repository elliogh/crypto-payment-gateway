-- +goose Up
CREATE TABLE partners (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    api_key_hash TEXT NOT NULL UNIQUE CHECK (length(api_key_hash) = 64),
    fee_bps INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000)
);

CREATE TABLE invoices (
    id UUID PRIMARY KEY,
    partner_id UUID NOT NULL REFERENCES partners(id),
    amount BIGINT NOT NULL CHECK (amount > 0),
    currency TEXT NOT NULL DEFAULT 'USDT' CHECK (currency = 'USDT'),
    deposit_address TEXT NOT NULL UNIQUE CHECK (deposit_address ~ '^0x[0-9a-f]{40}$'),
    fee_bps INTEGER NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
    status TEXT NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'PAID')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, partner_id)
);
CREATE INDEX invoices_partner ON invoices(partner_id);

CREATE TABLE invoice_idempotency (
    partner_id UUID NOT NULL REFERENCES partners(id),
    key TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    amount BIGINT NOT NULL CHECK (amount > 0),
    invoice_id UUID NOT NULL UNIQUE,
    PRIMARY KEY (partner_id, key),
    FOREIGN KEY (invoice_id, partner_id) REFERENCES invoices(id, partner_id) DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE ledger_accounts (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('ASSET', 'HOLD', 'PARTNER_AVAILABLE', 'INCOME', 'REFUND')),
    currency TEXT NOT NULL DEFAULT 'USDT' CHECK (currency = 'USDT'),
    partner_id UUID REFERENCES partners(id),
    invoice_id UUID,
    FOREIGN KEY (invoice_id, partner_id) REFERENCES invoices(id, partner_id),
    CHECK (
        (kind = 'INCOME' AND partner_id IS NULL AND invoice_id IS NULL) OR
        (kind = 'PARTNER_AVAILABLE' AND partner_id IS NOT NULL AND invoice_id IS NULL) OR
        (kind IN ('ASSET', 'HOLD', 'REFUND') AND partner_id IS NOT NULL AND invoice_id IS NOT NULL)
    ),
    UNIQUE NULLS NOT DISTINCT (kind, partner_id, invoice_id, currency),
    UNIQUE (id, currency)
);

CREATE TABLE ledger_operations (
    id UUID PRIMARY KEY,
    business_key TEXT NOT NULL UNIQUE CHECK (length(business_key) BETWEEN 1 AND 200),
    fingerprint TEXT NOT NULL CHECK (length(fingerprint) = 64),
    currency TEXT NOT NULL DEFAULT 'USDT' CHECK (currency = 'USDT'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, currency)
);

CREATE TABLE ledger_entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    operation_id UUID NOT NULL,
    account_id UUID NOT NULL,
    currency TEXT NOT NULL DEFAULT 'USDT' CHECK (currency = 'USDT'),
    side TEXT NOT NULL CHECK (side IN ('DEBIT', 'CREDIT')),
    amount NUMERIC(78,0) NOT NULL CHECK (amount > 0 AND (currency <> 'USDT' OR amount <= 9223372036854775807)),
    FOREIGN KEY (operation_id, currency) REFERENCES ledger_operations(id, currency),
    FOREIGN KEY (account_id, currency) REFERENCES ledger_accounts(id, currency)
);
CREATE INDEX ledger_entries_account ON ledger_entries(account_id);
CREATE INDEX ledger_entries_operation ON ledger_entries(operation_id);

-- +goose Down
DROP TABLE ledger_entries;
DROP TABLE ledger_operations;
DROP TABLE ledger_accounts;
DROP TABLE invoice_idempotency;
DROP TABLE invoices;
DROP TABLE partners;
