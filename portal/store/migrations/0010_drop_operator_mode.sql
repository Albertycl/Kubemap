-- Kubemap is read-only by design: the operator mode switch (0007) is gone,
-- and so is its table.
DROP TABLE IF EXISTS operator_mode;
