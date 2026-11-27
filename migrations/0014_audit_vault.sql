-- A payment request signed through QuorumVault (AG_SIGNER=vault) makes at most
-- one Vault attempt; that request's own audit row carries what it was: the Gate
-- credential, the Vault wallet, the payee, the amount, and Vault's answer. NULL
-- on every other row. Never the credential's sig, the token or a key.
ALTER TABLE audit_events
    ADD COLUMN vault_credential_id text,
    ADD COLUMN vault_wallet_id     text,
    ADD COLUMN vault_payee         text,
    ADD COLUMN vault_amount        text,
    ADD COLUMN vault_code          text,
    ADD COLUMN vault_field         text,
    ADD COLUMN vault_outcome       text;
-- No index here: a plain CREATE INDEX would block audit inserts on a live
-- table, and nothing queries by these columns yet.
