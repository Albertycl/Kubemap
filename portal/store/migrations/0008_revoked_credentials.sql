-- KM-94 (review-km-44 M3): credentials the admin revoked, by rotating a
-- cluster's credential or removing the cluster. Kept without a foreign key,
-- so they outlive a removed cluster: an agent that still presents one is
-- told "revoked" and stops, instead of retrying forever. reason is
-- 'rotated' or 'removed'.
CREATE TABLE revoked_credentials (
    credential_hash TEXT PRIMARY KEY,     -- hex SHA-256 of the credential
    cluster_id      TEXT NOT NULL,
    reason          TEXT NOT NULL,
    revoked_at      INTEGER NOT NULL
);
CREATE INDEX revoked_credentials_cluster ON revoked_credentials (cluster_id);

-- Removed clusters, so their node agents are told "revoked" too.
CREATE TABLE removed_clusters (
    cluster_id TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    removed_at INTEGER NOT NULL
);
