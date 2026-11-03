-- Audit (I2/I3) joins entries to holds and reads each account's latest entry;
-- both need these indexes to stay cheap as history grows.
CREATE INDEX ledger_entries_hold ON ledger_entries (hold_id) WHERE hold_id IS NOT NULL;
CREATE INDEX ledger_entries_account_id ON ledger_entries (account_id, id);
