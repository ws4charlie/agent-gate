-- Cedar shadow mode: next to the Go engine's decision, the verdict and rule
-- that the Cedar policies in git reached for the same request. Empty means
-- shadow was off for that row; 'error' means Cedar failed (the Go decision
-- was used either way).
ALTER TABLE policy_decisions ADD COLUMN cedar_verdict text NOT NULL DEFAULT '';
ALTER TABLE policy_decisions ADD COLUMN cedar_rule    text NOT NULL DEFAULT '';
