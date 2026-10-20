package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *SQLite) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

func (s *SQLite) CreateUser(ctx context.Context, u User) error {
	if u.Username == "" || u.PasswordHash == "" {
		return errors.New("store: create user: username and password hash are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, must_change_password, created_at) VALUES (?, ?, ?, ?)`,
		u.Username, u.PasswordHash, u.MustChangePassword, s.now().Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrExists
		}
		return fmt.Errorf("store: create user: %w", err)
	}
	return nil
}

func (s *SQLite) GetUser(ctx context.Context, username string) (User, error) {
	var u User
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT username, password_hash, must_change_password, created_at FROM users WHERE username = ?`,
		username).Scan(&u.Username, &u.PasswordHash, &u.MustChangePassword, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: get user: %w", err)
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	return u, nil
}

func (s *SQLite) SetPassword(ctx context.Context, username, passwordHash string, mustChange bool) error {
	if passwordHash == "" {
		return errors.New("store: set password: empty hash")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_password = ? WHERE username = ?`,
		passwordHash, mustChange, username)
	if err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE username = ?`, username); err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	return nil
}

func (s *SQLite) CreateSession(ctx context.Context, username string, expiresAt time.Time) (string, error) {
	token, err := newSecret()
	if err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashSecret(token), username, s.now().Unix(), expiresAt.Unix()); err != nil {
		return "", fmt.Errorf("store: create session: %w", err)
	}
	return token, nil
}

func (s *SQLite) GetSession(ctx context.Context, token string) (Session, error) {
	var sess Session
	var created, expires int64
	err := s.db.QueryRowContext(ctx,
		`SELECT username, created_at, expires_at FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		hashSecret(token), s.now().Unix()).Scan(&sess.Username, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	sess.CreatedAt = time.Unix(created, 0).UTC()
	sess.ExpiresAt = time.Unix(expires, 0).UTC()
	return sess, nil
}

func (s *SQLite) ExtendSession(ctx context.Context, token string, expiresAt time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET expires_at = ? WHERE token_hash = ? AND expires_at > ?`,
		expiresAt.Unix(), hashSecret(token), s.now().Unix())
	if err != nil {
		return fmt.Errorf("store: extend session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) DeleteSession(ctx context.Context, token string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashSecret(token)); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

func (s *SQLite) DeleteOtherSessions(ctx context.Context, username, keepToken string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE username = ? AND token_hash <> ?`, username, hashSecret(keepToken)); err != nil {
		return fmt.Errorf("store: delete other sessions: %w", err)
	}
	return nil
}

func (s *SQLite) PruneSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("store: prune sessions: %w", err)
	}
	return res.RowsAffected()
}

// TOTP is a user's two-factor state. SecretEnc is encrypted by the caller;
// the store never sees the clear secret.
type TOTP struct {
	SecretEnc []byte
	Enabled   bool
	LastStep  int64
}

// GetTOTP returns ErrNotFound when the user has no TOTP record.
func (s *SQLite) GetTOTP(ctx context.Context, username string) (TOTP, error) {
	var t TOTP
	err := s.db.QueryRowContext(ctx,
		`SELECT secret_enc, enabled, last_step FROM user_totp WHERE username = ?`, username).
		Scan(&t.SecretEnc, &t.Enabled, &t.LastStep)
	if errors.Is(err, sql.ErrNoRows) {
		return TOTP{}, ErrNotFound
	}
	if err != nil {
		return TOTP{}, fmt.Errorf("store: get totp: %w", err)
	}
	return t, nil
}

// SetPendingTOTP stores a new, not yet confirmed secret. It fails with
// ErrExists when TOTP is already enabled for the user.
func (s *SQLite) SetPendingTOTP(ctx context.Context, username string, secretEnc []byte) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user_totp (username, secret_enc, enabled, last_step, created_at) VALUES (?, ?, 0, 0, ?)
		 ON CONFLICT(username) DO UPDATE SET secret_enc = excluded.secret_enc, created_at = excluded.created_at WHERE enabled = 0`,
		username, secretEnc, s.now().Unix())
	if err != nil {
		return fmt.Errorf("store: set pending totp: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

// EnableTOTP turns a pending secret on, records the step of the confirming
// code and replaces the recovery codes (hex SHA-256 hashes) in one
// transaction. It fails with ErrNotFound without a pending secret.
func (s *SQLite) EnableTOTP(ctx context.Context, username string, step int64, codeHashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE user_totp SET enabled = 1, last_step = ? WHERE username = ? AND enabled = 0`, step, username)
	if err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_recovery_codes WHERE username = ?`, username); err != nil {
		return fmt.Errorf("store: enable totp: %w", err)
	}
	for _, h := range codeHashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO totp_recovery_codes (username, code_hash) VALUES (?, ?)`, username, h); err != nil {
			return fmt.Errorf("store: enable totp: %w", err)
		}
	}
	return tx.Commit()
}

// AdvanceTOTPStep records step as used. It returns false when the step is
// not newer than the last accepted one (a replay) or TOTP is not enabled.
// The check and the write are one statement, so two parallel requests with
// the same code cannot both pass.
func (s *SQLite) AdvanceTOTPStep(ctx context.Context, username string, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE user_totp SET last_step = ? WHERE username = ? AND enabled = 1 AND last_step < ?`, step, username, step)
	if err != nil {
		return false, fmt.Errorf("store: advance totp step: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UseRecoveryCode marks an unused recovery code (by hash) as used. It
// returns false when there is no such unused code.
func (s *SQLite) UseRecoveryCode(ctx context.Context, username, codeHash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE totp_recovery_codes SET used_at = ? WHERE username = ? AND code_hash = ? AND used_at IS NULL`,
		s.now().Unix(), username, codeHash)
	if err != nil {
		return false, fmt.Errorf("store: use recovery code: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountRecoveryCodes is the number of unused recovery codes.
func (s *SQLite) CountRecoveryCodes(ctx context.Context, username string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM totp_recovery_codes WHERE username = ? AND used_at IS NULL`, username).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count recovery codes: %w", err)
	}
	return n, nil
}

// DeleteTOTP removes the secret and the recovery codes. It reports whether a
// TOTP record existed.
func (s *SQLite) DeleteTOTP(ctx context.Context, username string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: delete totp: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM user_totp WHERE username = ?`, username)
	if err != nil {
		return false, fmt.Errorf("store: delete totp: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM totp_recovery_codes WHERE username = ?`, username); err != nil {
		return false, fmt.Errorf("store: delete totp: %w", err)
	}
	return n > 0, tx.Commit()
}

// CountEnabledTOTP is the number of users whose two-factor login is on.
func (s *SQLite) CountEnabledTOTP(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_totp WHERE enabled = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count enabled totp: %w", err)
	}
	return n, nil
}
