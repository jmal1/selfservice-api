-- Data repair is intentionally irreversible: lowering this quota would break
-- the synthetic-noop lifecycle and runner-smoke monitors again.
SELECT 1;
