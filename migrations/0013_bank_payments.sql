-- Bank rail (Mercury ACH): one row per payment, plus an append-only chain of
-- state changes. The row says where the payment is now; the events say how it
-- got there. A trigger rejects UPDATE and DELETE on the events.
CREATE TABLE bank_payments (
    id                  text PRIMARY KEY,
    account_id          text NOT NULL,
    agent_id            text NOT NULL,
    idem_key            text NOT NULL,
    fingerprint         text NOT NULL,
    recipient_id        text NOT NULL,
    amount_nanos        bigint NOT NULL CHECK (amount_nanos > 0),
    memo                text NOT NULL DEFAULT '',
    task_id             text NOT NULL DEFAULT '',
    -- bank_env is where the payment was made. A simulated return is allowed only on 'sandbox'.
    bank_env            text NOT NULL DEFAULT 'production' CHECK (bank_env IN ('sandbox','production')),
    -- received is the claim: the row exists before any money is held, so a
    -- second request with the same key finds it and never touches the hold.
    state               text NOT NULL CHECK (state IN ('received','refused','awaiting_gate_approval','awaiting_bank_approval',
                                                       'bank_approved','sent','settled','cancelled','failed','returned')),
    rule                text NOT NULL DEFAULT '',
    hold_id             text NOT NULL DEFAULT '',
    mercury_request_id  text NOT NULL DEFAULT '',
    mercury_txn_id      text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    sent_at             timestamptz,
    UNIQUE (account_id, idem_key)
);
CREATE INDEX bank_payments_open ON bank_payments (state) WHERE state NOT IN ('refused','cancelled','failed','returned');

CREATE TABLE bank_payment_events (
    payment_id  text NOT NULL REFERENCES bank_payments (id),
    seq         integer NOT NULL,
    from_state  text NOT NULL,
    to_state    text NOT NULL,
    actor       text NOT NULL,
    detail      text NOT NULL DEFAULT '{}',
    at          timestamptz NOT NULL,
    PRIMARY KEY (payment_id, seq)
);

CREATE FUNCTION bank_payment_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'bank_payment_events is append-only (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER bank_payment_events_no_update_delete
    BEFORE UPDATE OR DELETE ON bank_payment_events
    FOR EACH ROW EXECUTE FUNCTION bank_payment_events_append_only();
CREATE TRIGGER bank_payment_events_no_truncate
    BEFORE TRUNCATE ON bank_payment_events
    FOR EACH STATEMENT EXECUTE FUNCTION bank_payment_events_append_only();
