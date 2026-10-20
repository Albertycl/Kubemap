-- 0001: initial portal schema. Times are Unix seconds (UTC).
-- Secrets (import tokens, cluster credentials, passwords) are stored only as hashes.

CREATE TABLE clusters (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'active', 'disconnected')),
    created_at    INTEGER NOT NULL,
    accepted_at   INTEGER,
    last_seen     INTEGER,
    agent_version TEXT NOT NULL DEFAULT ''
);

CREATE TABLE nodes (
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    status     TEXT NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (cluster_id, name)
);

CREATE TABLE registration_tokens (
    token_hash TEXT PRIMARY KEY,          -- hex SHA-256 of the token
    cluster_id TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);

CREATE TABLE cluster_credentials (
    cluster_id      TEXT PRIMARY KEY REFERENCES clusters(id) ON DELETE CASCADE,
    credential_hash TEXT NOT NULL,        -- hex SHA-256 of the credential
    created_at      INTEGER NOT NULL
);

CREATE TABLE users (
    username      TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,          -- bcrypt
    created_at    INTEGER NOT NULL
);

CREATE TABLE audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    time       INTEGER NOT NULL,
    user       TEXT NOT NULL,
    cluster_id TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL
);
CREATE INDEX audit_log_time ON audit_log(time);

CREATE TABLE metric_rollups (
    cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    node        TEXT NOT NULL,
    bucket      INTEGER NOT NULL,         -- start of the time bucket
    cpu_pct     REAL NOT NULL,
    mem_pct     REAL NOT NULL,
    swap_pct    REAL NOT NULL,
    disk_pct    REAL NOT NULL,
    inodes_pct  REAL NOT NULL,
    psi_cpu     REAL NOT NULL,            -- PSI "some" avg10
    psi_memory  REAL NOT NULL,
    psi_io      REAL NOT NULL,
    PRIMARY KEY (cluster_id, node, bucket)
);
CREATE INDEX metric_rollups_bucket ON metric_rollups(bucket);
