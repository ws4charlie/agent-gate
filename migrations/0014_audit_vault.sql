-- One audit row per QuorumVault signing attempt (AG_SIGNER=vault) carries what
-- the attempt was: the Gate credential, the Vault wallet, the payee, the amount,
-- and Vault's answer. NULL on every other row. Never the credential's sig, the
-- token or a key.
ALTER TABLE audit_events
    ADD COLUMN vault_credential_id text,
    ADD COLUMN vault_wallet_id     text,
    ADD COLUMN vault_payee         text,
    ADD COLUMN vault_amount        text,
    ADD COLUMN vault_code          text,
    ADD COLUMN vault_field         text,
    ADD COLUMN vault_outcome       text;

CREATE INDEX audit_events_vault_credential_idx ON audit_events (vault_credential_id) WHERE vault_credential_id IS NOT NULL;
