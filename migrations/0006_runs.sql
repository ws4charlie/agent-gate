-- T5: runs (a unit of agent work that can be inspected or killed as one) and
-- the screening log.

-- A run groups the model calls, tool calls and payments of one job. The client
-- names it with X-AgentGate-Run; the gateway makes an id when the header is
-- absent. status: active | killed. A killed run is refused on its next
-- request and its in-flight calls are cancelled (I12).
CREATE TABLE runs (
    id         text PRIMARY KEY,
    agent_id   text NOT NULL REFERENCES agents (id),
    account_id text NOT NULL REFERENCES accounts (id),
    status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'killed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    killed_at  timestamptz
);
CREATE INDEX runs_agent ON runs (agent_id, created_at);

-- One row per model call, tool call or payment in a run. The row is written
-- when the request is admitted (status 'running') and closed when its audit
-- row is written. id is the order. The spend of a run is not stored here: it
-- is summed from ledger_entries through hold_id, so the ledger stays the one
-- source of truth for money (I2).
CREATE TABLE run_steps (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id      text NOT NULL REFERENCES runs (id),
    kind        text NOT NULL CHECK (kind IN ('model', 'tool', 'payment', 'mcp')),
    name        text NOT NULL DEFAULT '',
    request_id  text NOT NULL,
    hold_id     text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'running',
    cost_nanos  bigint NOT NULL DEFAULT 0,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz
);
CREATE INDEX run_steps_run ON run_steps (run_id, id);
CREATE UNIQUE INDEX run_steps_request ON run_steps (request_id);

-- One row per tool result the screen blocked. It keeps a SHA-256 of the
-- content and the rule that matched, never the content (I9): the content is
-- what the attacker wrote, and the log must not become a second way to carry it.
CREATE TABLE screen_events (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    request_id     text NOT NULL,
    run_id         text NOT NULL DEFAULT '',
    agent_id       text NOT NULL DEFAULT '',
    server         text NOT NULL,
    tool           text NOT NULL,
    rule           text NOT NULL,
    content_sha256 text NOT NULL,
    at             timestamptz NOT NULL
);
CREATE INDEX screen_events_request ON screen_events (request_id);
