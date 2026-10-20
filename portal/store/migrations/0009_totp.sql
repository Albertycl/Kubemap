-- Optional TOTP two-factor login (RFC 6238). The secret is stored encrypted
-- (AES-GCM, key file in the data directory), never in clear text. last_step
-- is the newest time step already accepted: a code of that step or an older
-- one is refused (replay protection).
CREATE TABLE user_totp (
    username   TEXT PRIMARY KEY,
    secret_enc BLOB NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 0,   -- 0 = enrolment started, not confirmed
    last_step  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- One-time recovery codes, stored as hex SHA-256 only.
CREATE TABLE totp_recovery_codes (
    username  TEXT NOT NULL,
    code_hash TEXT NOT NULL,
    used_at   INTEGER,
    PRIMARY KEY (username, code_hash)
);
