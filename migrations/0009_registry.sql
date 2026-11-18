-- The AI Registry: one catalogue of the models, tools and agents that are
-- allowed to exist, each with an owner and a purpose. Names are exact (a model
-- name, "server/tool" for a tool, an agent id), so "in use but not listed" is a
-- plain comparison. The registry records and reports; it blocks nothing.
CREATE TABLE registry_entries (
    kind       text NOT NULL CHECK (kind IN ('model', 'tool', 'agent')),
    name       text NOT NULL CHECK (name <> ''),
    owner      text NOT NULL CHECK (owner <> ''),
    purpose    text NOT NULL CHECK (purpose <> ''),
    status     text NOT NULL DEFAULT 'proposed' CHECK (status IN ('approved', 'proposed', 'deprecated', 'blocked')),
    notes      text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, name)
);
