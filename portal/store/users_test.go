package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	if n, err := s.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("CountUsers = %d, %v; want 0", n, err)
	}
	u := User{Username: "admin", PasswordHash: "$2a$12$fakehash", MustChangePassword: true}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, u); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate user: err = %v, want ErrExists", err)
	}
	got, err := s.GetUser(ctx, "admin")
	if err != nil || got.PasswordHash != u.PasswordHash || !got.MustChangePassword || !got.CreatedAt.Equal(t0) {
		t.Fatalf("GetUser = %+v, %v", got, err)
	}
	if _, err := s.GetUser(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: err = %v", err)
	}
	if err := s.SetPassword(ctx, "nobody", "h", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPassword unknown user: err = %v", err)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s, clk, _ := openTest(t)
	if err := s.CreateUser(ctx, User{Username: "admin", PasswordHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateSession(ctx, "admin", t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == tok || stored != hashSecret(tok) {
		t.Fatal("session token is not stored as its hash")
	}
	sess, err := s.GetSession(ctx, tok)
	if err != nil || sess.Username != "admin" || !sess.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("GetSession = %+v, %v", sess, err)
	}
	if _, err := s.GetSession(ctx, tok+"x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token: err = %v", err)
	}

	// Extend, then expire.
	if err := s.ExtendSession(ctx, tok, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	clk.add(90 * time.Minute)
	if _, err := s.GetSession(ctx, tok); err != nil {
		t.Fatalf("extended session expired early: %v", err)
	}
	clk.add(30 * time.Minute)
	if _, err := s.GetSession(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v", err)
	}
	if err := s.ExtendSession(ctx, tok, t0.Add(5*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("extending an expired session: err = %v", err)
	}
	if n, err := s.PruneSessions(ctx); err != nil || n != 1 {
		t.Fatalf("PruneSessions = %d, %v; want 1", n, err)
	}

	// Logout deletes one session; a password change deletes all of them.
	now := clk.now()
	a, _ := s.CreateSession(ctx, "admin", now.Add(time.Hour))
	b, _ := s.CreateSession(ctx, "admin", now.Add(time.Hour))
	if err := s.DeleteSession(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, a); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session still valid")
	}
	if err := s.SetPassword(ctx, "admin", "h2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, b); !errors.Is(err, ErrNotFound) {
		t.Fatal("session survived a password change")
	}
	if u, _ := s.GetUser(ctx, "admin"); u.PasswordHash != "h2" || u.MustChangePassword {
		t.Fatalf("after SetPassword: %+v", u)
	}
}

func TestTOTPStore(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openTest(t)
	if err := s.CreateUser(ctx, User{Username: "admin", PasswordHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTOTP(ctx, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no record: %v", err)
	}
	if err := s.EnableTOTP(ctx, "admin", 5, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enable without pending: %v", err)
	}
	if ok, _ := s.AdvanceTOTPStep(ctx, "admin", 9); ok {
		t.Fatal("advance without TOTP must fail")
	}
	// Pending secrets can be replaced until enabled.
	if err := s.SetPendingTOTP(ctx, "admin", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingTOTP(ctx, "admin", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetTOTP(ctx, "admin"); string(got.SecretEnc) != "two" || got.Enabled {
		t.Fatalf("pending = %+v", got)
	}
	if err := s.EnableTOTP(ctx, "admin", 100, []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPendingTOTP(ctx, "admin", []byte("three")); !errors.Is(err, ErrExists) {
		t.Fatalf("replace enabled secret: %v", err)
	}
	if got, _ := s.GetTOTP(ctx, "admin"); string(got.SecretEnc) != "two" || !got.Enabled || got.LastStep != 100 {
		t.Fatalf("enabled = %+v", got)
	}
	// Replay protection: only a newer step passes.
	for _, c := range []struct {
		step int64
		want bool
	}{{100, false}, {99, false}, {101, true}, {101, false}} {
		if ok, err := s.AdvanceTOTPStep(ctx, "admin", c.step); err != nil || ok != c.want {
			t.Fatalf("advance %d = %v, %v; want %v", c.step, ok, err, c.want)
		}
	}
	// Recovery codes work once.
	if n, _ := s.CountRecoveryCodes(ctx, "admin"); n != 2 {
		t.Fatalf("codes = %d", n)
	}
	if ok, _ := s.UseRecoveryCode(ctx, "admin", "h1"); !ok {
		t.Fatal("first use")
	}
	if ok, _ := s.UseRecoveryCode(ctx, "admin", "h1"); ok {
		t.Fatal("second use of a recovery code")
	}
	if ok, _ := s.UseRecoveryCode(ctx, "admin", "nope"); ok {
		t.Fatal("unknown recovery code")
	}
	if n, _ := s.CountRecoveryCodes(ctx, "admin"); n != 1 {
		t.Fatalf("codes left = %d", n)
	}
	if had, err := s.DeleteTOTP(ctx, "admin"); err != nil || !had {
		t.Fatalf("delete = %v, %v", had, err)
	}
	if n, _ := s.CountRecoveryCodes(ctx, "admin"); n != 0 {
		t.Fatal("codes survive delete")
	}
	if had, _ := s.DeleteTOTP(ctx, "admin"); had {
		t.Fatal("second delete reports a record")
	}
}
