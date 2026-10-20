-- KM-43: each node agent's version, from its register message, so the
-- portal can compare it with its own and offer the upgrade command.
ALTER TABLE nodes ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';
