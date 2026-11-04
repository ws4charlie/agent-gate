-- Gateway tables: client credentials, request claims, audit log.

-- I9: only the SHA-256 of a 256-bit random secret is stored. The secret is
-- shown once at creation and cannot be recovered.
CREATE TABLE api_keys (
    id         text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    account_id text NOT NULL REFERENCES accounts (id),
    key_hash   bytea NOT NULL UNIQUE,
    label      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

-- One row per (account, Idempotency-Key). The claim is how the gateway tells a
-- fresh request from a duplicate: ledger.Reserve replays the original hold for
-- a used key and cannot say which one it is. The loser gets 409 carrying the
-- winner's request id (PLAN flow step 7).
CREATE TABLE gateway_requests (
    account_id text NOT NULL REFERENCES accounts (id),
    idem_key   text NOT NULL,
    request_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, idem_key)
);

-- Audit log: append-only, one row per request outcome, failures and refusals
-- included. Bodies are NULL unless the operator opts in (I9).
CREATE TABLE audit_events (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id       text NOT NULL,
    account_id       text NOT NULL DEFAULT '',
    agent_id         text NOT NULL DEFAULT '',
    run_id           text NOT NULL DEFAULT '',
    kind             text NOT NULL DEFAULT 'model',
    model            text NOT NULL DEFAULT '',
    provider         text NOT NULL DEFAULT '',
    status           text NOT NULL,
    cost_source      text NOT NULL DEFAULT '',
    input_tokens       bigint NOT NULL DEFAULT 0,
    output_tokens      bigint NOT NULL DEFAULT 0,
    cache_read_tokens  bigint NOT NULL DEFAULT 0,
    cache_write_tokens bigint NOT NULL DEFAULT 0,
    upstream_id      text NOT NULL DEFAULT '',
    cost_nanos       bigint NOT NULL DEFAULT 0,
    unrecovered_nanos bigint NOT NULL DEFAULT 0,
    unmetered        boolean NOT NULL DEFAULT false,
    hold_id          text NOT NULL DEFAULT '',
    latency_ms       bigint NOT NULL DEFAULT 0,
    overhead_ms      bigint NOT NULL DEFAULT 0,
    err              text NOT NULL DEFAULT '',
    request_body     text,
    response_body    text,
    at               timestamptz NOT NULL
);
CREATE INDEX audit_events_at ON audit_events (at);
CREATE INDEX audit_events_request ON audit_events (request_id);

CREATE FUNCTION audit_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER audit_events_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
