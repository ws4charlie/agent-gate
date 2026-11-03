-- Ledger tables (PLAN "Ledger data model"). Invariants are enforced here, not
-- only in Go: I1 by CHECKs, I3/I4 by UNIQUEs, append-only by trigger.

CREATE TABLE accounts (
    id            text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name          text NOT NULL,
    balance_nanos bigint NOT NULL DEFAULT 0,
    held_nanos    bigint NOT NULL DEFAULT 0,
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'closed')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT held_nonnegative CHECK (held_nanos >= 0),          -- I1
    CONSTRAINT balance_covers_held CHECK (balance_nanos >= held_nanos) -- I1
);

CREATE TABLE holds (
    id                text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    account_id        text NOT NULL REFERENCES accounts (id),
    idem_key          text NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('model', 'tool', 'payment')),
    amount_nanos      bigint NOT NULL CHECK (amount_nanos >= 0),
    status            text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'settled', 'released')), -- I3
    expires_at        timestamptz NOT NULL,
    provider          text NOT NULL DEFAULT '',
    upstream_id       text NOT NULL DEFAULT '',
    settled_nanos     bigint NOT NULL DEFAULT 0 CHECK (settled_nanos >= 0),
    unrecovered_nanos bigint NOT NULL DEFAULT 0 CHECK (unrecovered_nanos >= 0), -- I7
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idem_key) -- I4
);
CREATE INDEX holds_active_expiry ON holds (expires_at) WHERE status = 'active'; -- I6 reconciler scan

CREATE TABLE ledger_entries (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    account_id     text NOT NULL REFERENCES accounts (id),
    hold_id        text REFERENCES holds (id),
    kind           text NOT NULL CHECK (kind IN ('deposit', 'hold', 'settle', 'release')),
    amount_nanos   bigint NOT NULL,
    balance_after  bigint NOT NULL,
    held_after     bigint NOT NULL,
    idem_key       text NOT NULL,
    memo           text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idem_key, kind) -- I4
);

-- Append-only: history is the evidence for I2, so it must not be rewritable.
CREATE FUNCTION ledger_entries_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_append_only();
