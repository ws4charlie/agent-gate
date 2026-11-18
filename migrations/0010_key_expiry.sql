-- API keys can expire and be rotated. NULL expiry means the key never expires,
-- so existing agents behave exactly as before.
--
-- key_expires_at: when the current key stops working.
-- prev_key_hash / prev_key_expires_at: the key that was replaced by a rotation.
-- It still works until prev_key_expires_at, so clients can switch without
-- downtime. At most one previous key is kept; the next rotation replaces it.
ALTER TABLE agents ADD COLUMN key_expires_at timestamptz;
ALTER TABLE agents ADD COLUMN prev_key_hash bytea UNIQUE;
ALTER TABLE agents ADD COLUMN prev_key_expires_at timestamptz;
