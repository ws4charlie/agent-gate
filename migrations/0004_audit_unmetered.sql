-- I6: a hold the reconciler released with no usage found is recorded in the
-- audit log as unmetered; this column keeps the amount that was returned.
ALTER TABLE audit_events ADD COLUMN unmetered_nanos bigint NOT NULL DEFAULT 0;
