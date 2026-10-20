-- 0003: what a registering cluster reports, shown on the Pending card (KM-17).

ALTER TABLE clusters ADD COLUMN registered_at INTEGER;          -- first successful register
ALTER TABLE clusters ADD COLUMN source_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN kubernetes_version TEXT NOT NULL DEFAULT '';
ALTER TABLE clusters ADD COLUMN node_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE clusters ADD COLUMN cluster_uid TEXT NOT NULL DEFAULT '';   -- kube-system namespace UID
