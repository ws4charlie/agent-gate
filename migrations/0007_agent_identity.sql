-- Workload identity: an agent can be known by a SPIFFE ID (the URI SAN of an
-- X.509 SVID the gateway verified over mutual TLS) instead of, or besides, its
-- API key. NULL means no SPIFFE identity; the UNIQUE constraint makes one ID
-- name at most one agent.
ALTER TABLE agents ADD COLUMN spiffe_id text UNIQUE;

-- The API key stays the fallback. Turning it off for an agent leaves the
-- SPIFFE ID as its only way in. Existing agents keep working unchanged.
ALTER TABLE agents ADD COLUMN api_key_enabled boolean NOT NULL DEFAULT true;
