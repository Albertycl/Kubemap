-- 0002: login sessions (KM-16). The session token is stored only as a hash.

ALTER TABLE users ADD COLUMN must_change_password INTEGER NOT NULL DEFAULT 0;

CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY,          -- hex SHA-256 of the session token
    username   TEXT NOT NULL REFERENCES users(username) ON DELETE CASCADE,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);
CREATE INDEX sessions_username ON sessions(username);
