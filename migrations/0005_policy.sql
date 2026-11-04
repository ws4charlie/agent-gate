-- T4: agents with scoped credentials, budget windows, the approval queue and
-- the decision log. api_keys (0003) stays as it was: it is the history the
-- agents table is seeded from, and nothing reads it after this migration.

-- One agent = one credential = one ledger account. I9: only the SHA-256 of a
-- 256-bit random secret is stored. `scope` is the policy.Scope JSON.
-- status: active | killed (reversible) | revoked (permanent).
CREATE TABLE agents (
    id         text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    account_id text NOT NULL REFERENCES accounts (id),
    name       text NOT NULL UNIQUE,
    key_hash   bytea NOT NULL UNIQUE,
    scope      jsonb NOT NULL,
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'killed', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT now(),
    status_at  timestamptz
);
CREATE INDEX agents_account ON agents (account_id);

-- T2 API keys keep working: same hash, same account, default scope (any model,
-- any tool, no limits, no payees). The agent id is the key id.
INSERT INTO agents (id, account_id, name, key_hash, scope, status, created_at, status_at)
SELECT id, account_id, 'key-' || id, key_hash, '{"models":["*"],"tools":["*"]}'::jsonb,
       CASE WHEN revoked_at IS NULL THEN 'active' ELSE 'revoked' END, created_at, revoked_at
FROM api_keys;

-- Spend counters per window. window_key is 'day:YYYY-MM-DD' (UTC) or
-- 'task:<id>'. The budget check is one conditional upsert on this row, as the
-- solvency check is one conditional update on accounts (I5 for budgets), so
-- there is no read-then-write to race.
CREATE TABLE budget_windows (
    agent_id    text NOT NULL REFERENCES agents (id),
    window_key  text NOT NULL,
    spent_nanos bigint NOT NULL DEFAULT 0 CHECK (spent_nanos >= 0),
    PRIMARY KEY (agent_id, window_key)
);

-- One row per admitted request, keyed by the hold that funds it. It records
-- which windows the request was counted in, so settling or releasing the hold
-- (from the request handler or from the reconciler) can correct them: the
-- window holds the estimate until the real cost is known. It is also the
-- velocity ledger: the sliding minute is the sum of amount_nanos over recent
-- rows. status open -> closed happens once.
CREATE TABLE budget_charges (
    hold_id      text PRIMARY KEY REFERENCES holds (id),
    agent_id     text NOT NULL REFERENCES agents (id),
    day_key      text NOT NULL,
    task_key     text NOT NULL,
    amount_nanos bigint NOT NULL CHECK (amount_nanos >= 0),
    status       text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    at           timestamptz NOT NULL
);
CREATE INDEX budget_charges_velocity ON budget_charges (agent_id, at);

-- Approval queue. A request over the agent's threshold keeps its hold (the
-- funds stay locked) and waits here. The row pins what a human approved:
-- `fingerprint` covers kind, model/tool, payee, amount, network, task and body.
-- pending -> approved | denied | expired; approved -> consumed (once).
CREATE TABLE approvals (
    id          text PRIMARY KEY DEFAULT gen_random_uuid()::text,
    agent_id    text NOT NULL REFERENCES agents (id),
    account_id  text NOT NULL REFERENCES accounts (id),
    idem_key    text NOT NULL,
    request_id  text NOT NULL,
    kind        text NOT NULL,
    subject     text NOT NULL DEFAULT '',
    payee       text NOT NULL DEFAULT '',
    amount_nanos bigint NOT NULL,
    task_id     text NOT NULL DEFAULT '',
    fingerprint text NOT NULL,
    hold_id     text NOT NULL REFERENCES holds (id),
    status      text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'approved', 'denied', 'expired', 'consumed')),
    decided_by  text NOT NULL DEFAULT '',
    decided_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    UNIQUE (agent_id, idem_key)
);
CREATE INDEX approvals_status ON approvals (status, created_at);

-- Every policy decision, allow included, with the rule that decided it.
-- Append-only like audit_events (T2). request_id joins to audit_events.
CREATE TABLE policy_decisions (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id   text NOT NULL,
    agent_id     text NOT NULL,
    account_id   text NOT NULL DEFAULT '',
    kind         text NOT NULL,
    subject      text NOT NULL DEFAULT '',
    payee        text NOT NULL DEFAULT '',
    task_id      text NOT NULL DEFAULT '',
    amount_nanos bigint NOT NULL DEFAULT 0,
    decision     text NOT NULL,
    rule         text NOT NULL,
    reason       text NOT NULL DEFAULT '',
    approval_id  text NOT NULL DEFAULT '',
    at           timestamptz NOT NULL
);
CREATE INDEX policy_decisions_request ON policy_decisions (request_id);
CREATE INDEX policy_decisions_agent ON policy_decisions (agent_id, at);

CREATE FUNCTION policy_decisions_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'policy_decisions is append-only (% rejected)', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER policy_decisions_no_update_delete
    BEFORE UPDATE OR DELETE ON policy_decisions
    FOR EACH ROW EXECUTE FUNCTION policy_decisions_append_only();
CREATE TRIGGER policy_decisions_no_truncate
    BEFORE TRUNCATE ON policy_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION policy_decisions_append_only();
