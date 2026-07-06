-- Accounts: money is BIGINT cents; the CHECK is the database-level last line
-- of defense against negative balances, independent of application code.
CREATE TABLE accounts (
    id            UUID PRIMARY KEY,
    number        TEXT NOT NULL UNIQUE,
    holder        TEXT NOT NULL,
    balance_cents BIGINT NOT NULL CHECK (balance_cents >= 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Transactions: failed transfers (business decisions like insufficient funds)
-- are persisted too, so idempotent replays of a 402 are deterministic.
CREATE TABLE transactions (
    id                UUID PRIMARY KEY,
    source_account_id UUID NOT NULL REFERENCES accounts (id),
    dest_account_id   UUID NOT NULL REFERENCES accounts (id),
    amount_cents      BIGINT NOT NULL CHECK (amount_cents > 0),
    description       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL CHECK (status IN ('completed', 'failed')),
    failure_reason    TEXT NOT NULL DEFAULT '',
    end_to_end_id     TEXT NOT NULL UNIQUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_transactions_source ON transactions (source_account_id);
CREATE INDEX idx_transactions_dest   ON transactions (dest_account_id);

-- Idempotency keys: the UNIQUE index on key is what makes concurrent
-- duplicates safe — INSERT ... ON CONFLICT DO NOTHING inside the transfer
-- transaction serializes racers; the loser reads the winner's result.
CREATE TABLE idempotency_keys (
    id             UUID PRIMARY KEY,
    key            TEXT NOT NULL UNIQUE,
    request_hash   TEXT NOT NULL,
    transaction_id UUID REFERENCES transactions (id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Outbox: events are inserted in the SAME transaction as the transfer and
-- delivered asynchronously by the worker (at-least-once).
CREATE TABLE events (
    id              UUID PRIMARY KEY,
    type            TEXT NOT NULL,
    payload         JSONB NOT NULL,
    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index keeps the pending-events poll cheap regardless of history size.
CREATE INDEX idx_events_pending ON events (next_attempt_at) WHERE delivered_at IS NULL;
