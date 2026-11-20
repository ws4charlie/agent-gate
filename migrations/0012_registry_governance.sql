-- The AI Registry governs: with the gateway's --registry-enforce on, a model or
-- tool that is not approved here is refused (rule registry_unapproved).
-- risk_tier and version describe the entry; registry_changes is the audit trail
-- of every creation, status change and removal.
ALTER TABLE registry_entries ADD COLUMN risk_tier text NOT NULL DEFAULT 'medium' CHECK (risk_tier IN ('low', 'medium', 'high'));
ALTER TABLE registry_entries ADD COLUMN version   text NOT NULL DEFAULT '';

CREATE TABLE registry_changes (
    id          bigserial PRIMARY KEY,
    kind        text NOT NULL,
    name        text NOT NULL,
    from_status text NOT NULL DEFAULT '',  -- '' when the entry was created
    to_status   text NOT NULL DEFAULT '',  -- '' when the entry was removed
    actor       text NOT NULL DEFAULT '',
    note        text NOT NULL DEFAULT '',
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX registry_changes_item ON registry_changes (kind, name, at DESC);
