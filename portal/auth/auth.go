// Package auth is the portal login: one admin account with a bcrypt
// password, and server-side sessions in a cookie.
//
// First start: if there are no users, Bootstrap creates "admin" with a random
// password, writes it to <data dir>/initial-admin-password (mode 0600) and
// flags it must-change. The password is never logged. After the admin sets a
// new password the file is deleted.
//
// API (JSON):
//
//	POST /api/login     {"username","password"} -> sets the session cookie
//	POST /api/logout
//	GET  /api/me        -> {"username","mustChangePassword"} or 401
//	POST /api/password  {"currentPassword","newPassword"}
//
// Require wraps every other /api route. While the password must change, it
// allows only /api/me, /api/logout and /api/password.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"kubemap/portal/store"
)

const (
	AdminUser = "admin"
	// CookieName is the session cookie. It is HttpOnly, Secure and
	// SameSite=Strict.
	CookieName = "kubemap_session"
	// IdleTimeout ends a session that is not used for this long.
	IdleTimeout = 12 * time.Hour
	// MaxSessionAge ends every session this long after login.
	MaxSessionAge = 7 * 24 * time.Hour
	// MinPasswordLen and MaxPasswordBytes bound new passwords. bcrypt uses
	// only the first 72 bytes, so longer ones are refused, not truncated.
	MinPasswordLen   = 12
	MaxPasswordBytes = 72
	BcryptCost       = 12

	InitialPasswordFile = "initial-admin-password"

	// MaxConcurrentHashes is how many password checks run at once. Others
	// wait up to hashWait, then get "busy".
	MaxConcurrentHashes = 2
)

// hashWait is how long a request waits for a free password-check slot.
// A variable so tests can shorten it.
var hashWait = 3 * time.Second

var ErrWeakPassword = fmt.Errorf("password must be %d to %d bytes long", MinPasswordLen, MaxPasswordBytes)

// Service is the login service.
type Service struct {
	store   store.Store
	log     *slog.Logger
	now     func() time.Time
	dataDir string
	limiter *limiter
	// backoff slows guessing at one username from many addresses.
	backoff *backoff
	// totpFails counts wrong second-factor codes per user, from any address.
	totpFails *limiter
	// hashSlots bounds concurrent bcrypt work, so unauthenticated requests
	// cannot use up the host's CPU.
	hashSlots chan struct{}
	// compare is bcrypt.CompareHashAndPassword; tests count the calls.
	compare func(hash, password []byte) error
	cost    int // bcrypt cost for new hashes; tests lower it
	// dummyHash is compared against when the user does not exist, so a
	// wrong username takes as long as a wrong password. It is made on first
	// use, with the current cost.
	dummyOnce sync.Once
	dummyHash []byte
	// key is the TOTP secret encryption key, read from the data dir on first use.
	keyMu sync.Mutex
	key   []byte
}

// New returns a Service. dataDir is where the initial password file lives.
func New(st store.Store, log *slog.Logger, dataDir string) (*Service, error) {
	return &Service{
		store:     st,
		log:       log,
		now:       time.Now,
		dataDir:   dataDir,
		limiter:   newLimiter(),
		backoff:   newBackoff(),
		totpFails: newLimiter(),
		hashSlots: make(chan struct{}, MaxConcurrentHashes),
		compare:   bcrypt.CompareHashAndPassword,
		cost:      BcryptCost,
	}, nil
}

// WithBcryptCost sets the bcrypt cost for new hashes. Only tests use it, to
// run fast; the portal always uses BcryptCost.
func (s *Service) WithBcryptCost(cost int) *Service {
	s.cost = cost
	return s
}

func (s *Service) dummy() []byte {
	s.dummyOnce.Do(func() {
		s.dummyHash, _ = bcrypt.GenerateFromPassword([]byte("kubemap-dummy-password"), s.cost)
	})
	return s.dummyHash
}

// InitialPasswordPath is where Bootstrap writes the first admin password.
func (s *Service) InitialPasswordPath() string {
	return filepath.Join(s.dataDir, InitialPasswordFile)
}

// Bootstrap creates the admin account on first start. It reports whether it
// created one; the password is then in InitialPasswordPath.
func (s *Service) Bootstrap(ctx context.Context) (bool, error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	pw, err := randomPassword()
	if err != nil {
		return false, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), s.cost)
	if err != nil {
		return false, err
	}
	// Write the file first: if that fails there is no account nobody can use.
	if err := os.WriteFile(s.InitialPasswordPath(), []byte(pw+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("auth: write initial password: %w", err)
	}
	if err := s.store.CreateUser(ctx, store.User{Username: AdminUser, PasswordHash: string(hash), MustChangePassword: true}); err != nil {
		os.Remove(s.InitialPasswordPath())
		return false, err
	}
	return true, nil
}

// ResetAdminPassword sets a new random admin password (must change at next
// login), ends all admin sessions and returns the password. It is for the
// "reset-admin-password" command, which prints it to the terminal.
func (s *Service) ResetAdminPassword(ctx context.Context) (string, error) {
	pw, err := randomPassword()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), s.cost)
	if err != nil {
		return "", err
	}
	err = s.store.SetPassword(ctx, AdminUser, string(hash), true)
	if errors.Is(err, store.ErrNotFound) {
		err = s.store.CreateUser(ctx, store.User{Username: AdminUser, PasswordHash: string(hash), MustChangePassword: true})
	}
	if err != nil {
		return "", err
	}
	s.audit(ctx, "cli", "password.reset", AdminUser, "ok")
	return pw, nil
}

func randomPassword() (string, error) {
	b := make([]byte, 18) // 144 bits, 24 characters
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func checkNewPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLen || len(pw) > MaxPasswordBytes {
		return ErrWeakPassword
	}
	return nil
}

// authenticate checks a username and password in roughly constant time.
func (s *Service) authenticate(ctx context.Context, username, password string) (store.User, bool, error) {
	u, err := s.store.GetUser(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		s.compare(s.dummy(), []byte(password))
		return store.User{}, false, nil
	}
	if err != nil {
		return store.User{}, false, err
	}
	if s.compare([]byte(u.PasswordHash), []byte(password)) != nil {
		return store.User{}, false, nil
	}
	return u, true, nil
}

// changePassword checks the current password and stores the new one. All
// sessions of the user end; the caller issues a fresh one.
func (s *Service) changePassword(ctx context.Context, username, current, next string) error {
	if _, ok, err := s.authenticate(ctx, username, current); err != nil {
		return err
	} else if !ok {
		return errWrongPassword
	}
	if err := checkNewPassword(next); err != nil {
		return err
	}
	if next == current {
		return errSamePassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(next), s.cost)
	if err != nil {
		return err
	}
	if err := s.store.SetPassword(ctx, username, string(hash), false); err != nil {
		return err
	}
	if username == AdminUser {
		if err := os.Remove(s.InitialPasswordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("could not delete the initial password file", "path", s.InitialPasswordPath(), "err", err)
		}
	}
	return nil
}

// errBusy means all password-check slots stayed full for hashWait.
var errBusy = errors.New("auth: too many password checks at once")

// withHashSlot runs f while holding one of MaxConcurrentHashes slots.
func (s *Service) withHashSlot(ctx context.Context, f func() error) error {
	t := time.NewTimer(hashWait)
	defer t.Stop()
	select {
	case s.hashSlots <- struct{}{}:
	case <-t.C:
		return errBusy
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.hashSlots }()
	return f()
}

var (
	errWrongPassword = errors.New("current password is wrong")
	errSamePassword  = errors.New("new password must differ from the current one")
)

// sessionExpiry is the new expiry for a session created at created: the idle
// timeout from now, capped at MaxSessionAge after login.
func (s *Service) sessionExpiry(created time.Time) time.Time {
	exp := s.now().Add(IdleTimeout)
	if limit := created.Add(MaxSessionAge); exp.After(limit) {
		return limit
	}
	return exp
}

func (s *Service) audit(ctx context.Context, user, action, target, result string) {
	if err := s.store.WriteAudit(ctx, store.AuditEntry{User: user, Action: action, Target: target, Result: result}); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}
