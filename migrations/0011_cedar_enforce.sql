-- Cedar enforce mode: when Cedar decides, decision/rule hold Cedar's verdict and
-- these two hold what the Go engine said, so `agentgate cedar shadow` can still
-- measure agreement. Empty means shadow mode: decision/rule are the Go engine's.
ALTER TABLE policy_decisions ADD COLUMN go_verdict text NOT NULL DEFAULT '';
ALTER TABLE policy_decisions ADD COLUMN go_rule    text NOT NULL DEFAULT '';
