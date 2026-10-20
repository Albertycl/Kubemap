package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 vectors (secret "12345678901234567890"). The RFC
// lists 8 digits; the 6-digit code is the last 6 of them.
func TestRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		if got := totpCode(secret, v.unix/30); got != v.code {
			t.Errorf("T=%d: %s, want %s", v.unix, got, v.code)
		}
	}
}

func TestTOTPWindow(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1111111111, 0)
	cur := now.Unix() / 30
	for _, c := range []struct {
		step int64
		ok   bool
	}{{cur - 2, false}, {cur - 1, true}, {cur, true}, {cur + 1, true}, {cur + 2, false}} {
		step, ok := totpMatch(secret, totpCode(secret, c.step), now)
		if ok != c.ok || (ok && step != c.step) {
			t.Errorf("step %+d: got %d, %v; want ok=%v", c.step-cur, step, ok, c.ok)
		}
	}
}

func TestTOTPURI(t *testing.T) {
	u := totpURI([]byte("12345678901234567890"), "admin")
	for _, want := range []string{"otpauth://totp/Kubemap:admin?", "secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", "issuer=Kubemap", "digits=6", "period=30", "algorithm=SHA1"} {
		if !strings.Contains(u, want) {
			t.Errorf("%q missing from %s", want, u)
		}
	}
}

// totpEnv is a logged-in admin (password changed) who may start TOTP setup.
type totpEnv struct {
	*env
	cookie string
	pw     string
}

func newTOTPEnv(t *testing.T) *totpEnv {
	e := newEnv(t)
	first := e.bootstrap()
	c := e.login("admin", first)
	pw := "a-good-new-password"
	w := e.do("POST", "/api/password", c, map[string]string{"currentPassword": first, "newPassword": pw})
	if w.Code != 200 {
		t.Fatalf("password change %d", w.Code)
	}
	return &totpEnv{env: e, cookie: sessionCookie(t, w).Value, pw: pw}
}

type setupAnswer struct{ Secret, URI string }

func (e *totpEnv) setup() (secret []byte) {
	e.t.Helper()
	w := e.do("POST", "/api/totp/setup", e.cookie, map[string]string{"password": e.pw})
	if w.Code != 200 {
		e.t.Fatalf("setup %d %s", w.Code, w.Body)
	}
	var a setupAnswer
	json.Unmarshal(w.Body.Bytes(), &a)
	secret, err := b32.DecodeString(a.Secret)
	if err != nil || len(secret) != 20 || !strings.HasPrefix(a.URI, "otpauth://totp/") || !strings.Contains(a.URI, a.Secret) {
		e.t.Fatalf("setup answer %+v, %v", a, err)
	}
	return secret
}

// code returns the code for the clock of the test plus n steps.
func (e *totpEnv) code(secret []byte, n int64) string {
	return totpCode(secret, e.now.Unix()/30+n)
}

func (e *totpEnv) enable(secret []byte) []string {
	e.t.Helper()
	w := e.do("POST", "/api/totp/enable", e.cookie, map[string]string{"code": e.code(secret, 0)})
	if w.Code != 200 {
		e.t.Fatalf("enable %d %s", w.Code, w.Body)
	}
	var a struct{ RecoveryCodes []string }
	json.Unmarshal(w.Body.Bytes(), &a)
	if len(a.RecoveryCodes) != 8 {
		e.t.Fatalf("recovery codes %v", a.RecoveryCodes)
	}
	return a.RecoveryCodes
}

func (e *totpEnv) loginWith(code string) *httptest.ResponseRecorder {
	return e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": e.pw, "code": code})
}

func (e *totpEnv) audits() string {
	rows, err := e.st.ListAudit(context.Background(), 100)
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.User + " " + r.Action + " " + r.Result + "\n")
	}
	return b.String()
}

func TestTOTPEnrolLoginReplayAndRecovery(t *testing.T) {
	e := newTOTPEnv(t)

	// Status: off. Setup needs the password.
	if w := e.do("GET", "/api/totp", e.cookie, nil); !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("status %s", w.Body)
	}
	if w := e.do("POST", "/api/totp/setup", e.cookie, map[string]string{"password": "wrong"}); w.Code != http.StatusForbidden {
		t.Fatalf("setup with wrong password: %d", w.Code)
	}
	secret := e.setup()

	// Not on yet: login still needs no code. A wrong code does not enable it.
	e.login("admin", e.pw)
	if w := e.do("POST", "/api/totp/enable", e.cookie, map[string]string{"code": "000000"}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bad_totp") {
		t.Fatalf("enable with wrong code: %d %s", w.Code, w.Body)
	}
	codes := e.enable(secret)
	if w := e.do("POST", "/api/totp/setup", e.cookie, map[string]string{"password": e.pw}); w.Code != http.StatusConflict {
		t.Fatalf("setup while enabled: %d (the secret must not be shown again)", w.Code)
	}
	if w := e.do("GET", "/api/totp", e.cookie, nil); !strings.Contains(w.Body.String(), `"enabled":true`) || !strings.Contains(w.Body.String(), `"recoveryCodesLeft":8`) {
		t.Fatalf("status %s", w.Body)
	}
	if w := e.do("GET", "/api/me", e.cookie, nil); !strings.Contains(w.Body.String(), `"totpEnabled":true`) {
		t.Fatalf("me %s", w.Body)
	}

	// The secret is encrypted in the database and no API returns it.
	rec, err := e.st.GetTOTP(context.Background(), "admin")
	if err != nil || strings.Contains(string(rec.SecretEnc), string(secret)) || strings.Contains(string(rec.SecretEnc), b32.EncodeToString(secret)) {
		t.Fatalf("secret stored in clear? %v", err)
	}
	if st, err := os.Stat(filepath.Join(e.dir, totpKeyFile)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	for _, p := range []string{"/api/me", "/api/totp"} {
		if w := e.do("GET", p, e.cookie, nil); strings.Contains(w.Body.String(), b32.EncodeToString(secret)) {
			t.Fatalf("%s returns the secret", p)
		}
	}

	// Login: password alone is not enough, and the answer comes only after a right password.
	w := e.loginWith("")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "totp_required") {
		t.Fatalf("no code: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "wrong", "code": ""}); !strings.Contains(w.Body.String(), "bad_credentials") {
		t.Fatalf("wrong password must not hint at TOTP: %s", w.Body)
	}
	if w := e.loginWith("123456"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "bad_totp") {
		t.Fatalf("wrong code: %d %s", w.Code, w.Body)
	}
	// The step of the confirming code (now) was used by enable: replay refused.
	if w := e.loginWith(e.code(secret, 0)); w.Code != http.StatusUnauthorized {
		t.Fatalf("code of the enrol step reused: %d", w.Code)
	}
	// The next step is fine, once.
	next := e.code(secret, 1)
	if w := e.loginWith(next); w.Code != 200 {
		t.Fatalf("good code: %d %s", w.Code, w.Body)
	}
	if w := e.loginWith(next); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code: %d", w.Code)
	}
	// An older step in the window is replay too.
	if w := e.loginWith(e.code(secret, -1)); w.Code != http.StatusUnauthorized {
		t.Fatalf("older step after a newer one: %d", w.Code)
	}

	// Recovery code works once, any case and spacing.
	rc := strings.ToLower(strings.ReplaceAll(codes[0], "-", " "))
	if w := e.loginWith(rc); w.Code != 200 {
		t.Fatalf("recovery code: %d %s", w.Code, w.Body)
	}
	if w := e.loginWith(codes[0]); w.Code != http.StatusUnauthorized {
		t.Fatalf("recovery code reused: %d", w.Code)
	}
	if n, _ := e.st.CountRecoveryCodes(context.Background(), "admin"); n != 7 {
		t.Fatalf("codes left %d", n)
	}
	// Recovery codes are stored hashed.
	if h := hashRecovery(normalizeCode(codes[1])); h == normalizeCode(codes[1]) || len(h) != 64 {
		t.Fatal("recovery hash")
	}

	a := e.audits()
	for _, want := range []string{"admin totp.enable failed", "admin totp.enable ok", "admin login.totp failed", "admin totp.recovery ok"} {
		if !strings.Contains(a, want) {
			t.Errorf("audit log lacks %q:\n%s", want, a)
		}
	}
}

func TestTOTPFailuresUseTheLoginLimit(t *testing.T) {
	e := newTOTPEnv(t)
	secret := e.setup()
	e.enable(secret)
	for i := 0; i < MaxFailures; i++ {
		if w := e.loginWith("111111"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := e.loginWith(e.code(secret, 1))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong codes, even a right one: %d, want 429", MaxFailures, w.Code)
	}
	e.now = e.now.Add(FailureWindow)
	if w := e.loginWith(e.code(secret, 1)); w.Code != 200 {
		t.Fatalf("after the window: %d %s", w.Code, w.Body)
	}
}

func TestTOTPDisable(t *testing.T) {
	e := newTOTPEnv(t)
	secret := e.setup()
	codes := e.enable(secret)

	// Needs password and a code.
	if w := e.do("POST", "/api/totp/disable", e.cookie, map[string]string{"password": "wrong", "code": e.code(secret, 1)}); w.Code != http.StatusForbidden {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := e.do("POST", "/api/totp/disable", e.cookie, map[string]string{"password": e.pw, "code": "123456"}); w.Code != http.StatusForbidden {
		t.Fatalf("wrong code: %d", w.Code)
	}
	if w := e.do("POST", "/api/totp/disable", e.cookie, map[string]string{"password": e.pw, "code": codes[2]}); w.Code != http.StatusNoContent {
		t.Fatalf("disable with a recovery code: %d", w.Code)
	}
	if w := e.loginWith(""); w.Code != 200 {
		t.Fatalf("login after disable: %d %s", w.Code, w.Body)
	}
	a := e.audits()
	if !strings.Contains(a, "admin totp.disable failed") || !strings.Contains(a, "admin totp.disable ok") {
		t.Errorf("audit:\n%s", a)
	}
	// Starting again gives a fresh secret.
	if s2 := e.setup(); string(s2) == string(secret) {
		t.Fatal("secret reused")
	}
}

func TestDisableTOTPCommand(t *testing.T) {
	e := newTOTPEnv(t)
	e.enable(e.setup())
	had, err := e.svc.DisableTOTP(context.Background(), AdminUser)
	if err != nil || !had {
		t.Fatalf("DisableTOTP = %v, %v", had, err)
	}
	if w := e.loginWith(""); w.Code != 200 {
		t.Fatalf("login after disable-totp: %d", w.Code)
	}
	if had, _ := e.svc.DisableTOTP(context.Background(), AdminUser); had {
		t.Fatal("second call reports a record")
	}
	if !strings.Contains(e.audits(), "cli totp.disable ok") {
		t.Errorf("audit:\n%s", e.audits())
	}
}

func TestTOTPNeedsLoginAndPasswordChange(t *testing.T) {
	e := newEnv(t)
	first := e.bootstrap()
	for _, r := range [][2]string{{"GET", "/api/totp"}, {"POST", "/api/totp/setup"}, {"POST", "/api/totp/enable"}, {"POST", "/api/totp/disable"}} {
		if w := e.do(r[0], r[1], "", map[string]string{}); w.Code != http.StatusUnauthorized {
			t.Errorf("%v without login: %d", r, w.Code)
		}
	}
	c := e.login("admin", first) // still must change the password
	if w := e.do("POST", "/api/totp/setup", c, map[string]string{"password": first}); w.Code != http.StatusForbidden {
		t.Errorf("setup before the password change: %d", w.Code)
	}
}

func TestTOTPSecretBoundToUser(t *testing.T) {
	e := newTOTPEnv(t)
	enc, err := e.svc.sealSecret("admin", []byte("12345678901234567890"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.openSecret("mallory", enc); err == nil {
		t.Fatal("secret opened for another user")
	}
	if b, err := e.svc.openSecret("admin", enc); err != nil || string(b) != "12345678901234567890" {
		t.Fatalf("round trip: %q %v", b, err)
	}
}

func TestTOTPAccountWideLock(t *testing.T) {
	e := newTOTPEnv(t)
	secret := e.setup()
	codes := e.enable(secret)
	// Wrong codes from many addresses (so no per-address limit trips).
	for i := 0; i < MaxTOTPFailures; i++ {
		e.remote = "192.0.2." + string(rune('1'+i)) + ":4000"
		if w := e.loginWith("111111"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	e.remote = "198.51.100.77:1"
	for name, code := range map[string]string{"totp": e.code(secret, 1), "recovery": codes[0]} {
		w := e.loginWith(code)
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "totp_locked") || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%s while locked: %d %s", name, w.Code, w.Body)
		}
	}
	if !strings.Contains(e.audits(), "admin totp_locked") {
		t.Errorf("no totp_locked audit:\n%s", e.audits())
	}
	// A valid session is not affected.
	if w := e.do("GET", "/api/me", e.cookie, nil); w.Code != 200 {
		t.Fatalf("session during lock: %d", w.Code)
	}
	// The lock ends by itself; the recovery code was not used up.
	e.now = e.now.Add(FailureWindow)
	if w := e.loginWith(codes[0]); w.Code != 200 {
		t.Fatalf("after the wait: %d %s", w.Code, w.Body)
	}
}

func TestTOTPLockCountsRecoveryCodes(t *testing.T) {
	e := newTOTPEnv(t)
	e.enable(e.setup())
	for i := 0; i < MaxTOTPFailures; i++ {
		e.remote = "192.0.2." + string(rune('1'+i)) + ":4000"
		e.loginWith("AAAA-BBBB-CCCC")
	}
	if w := e.loginWith("123456"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("after wrong recovery codes: %d", w.Code)
	}
}

func TestSharedClientKeyHasHigherLimit(t *testing.T) {
	n := 0
	bad := func(e *totpEnv, count int, xff string) int {
		last := 0
		for i := 0; i < count; i++ {
			e.remote = "10.42.0.7:4000" // Traefik / a node: a trusted proxy
			hdr := []string{}
			if xff != "" {
				hdr = []string{"X-Forwarded-For", xff}
			}
			n++ // a new name each time: the per-name backoff would sleep in real time
			w := e.do("POST", "/api/login", "", map[string]string{"username": "nobody" + strconv.Itoa(n), "password": "wrong"}, hdr...)
			last = w.Code
		}
		return last
	}
	// Junk XFF and no XFF (SNAT): 5 bad logins do not block, 50 do.
	for _, xff := range []string{"junk", ""} {
		e := newTOTPEnv(t)
		if c := bad(e, MaxFailures+1, xff); c != http.StatusUnauthorized {
			t.Fatalf("xff %q: after %d bad logins got %d, want 401", xff, MaxFailures+1, c)
		}
		e.remote = "10.42.0.7:4000"
		if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": e.pw}); w.Code != 200 {
			t.Fatalf("admin through a shared address: %d", w.Code)
		}
		if c := bad(e, SharedMaxFailures+1, xff); c != http.StatusTooManyRequests {
			t.Fatalf("xff %q: after %d more bad logins got %d, want 429", xff, SharedMaxFailures+1, c)
		}
	}
	// A real client address behind the proxy keeps the strict limit.
	e := newTOTPEnv(t)
	if c := bad(e, MaxFailures+1, "198.51.100.9"); c != http.StatusTooManyRequests {
		t.Fatalf("real client: %d, want 429", c)
	}
}

func TestEnableTOTPEndsOtherSessions(t *testing.T) {
	e := newTOTPEnv(t)
	other := e.login("admin", e.pw)
	secret := e.setup()
	e.enable(secret)
	if w := e.do("GET", "/api/me", other, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("other session after enable: %d, want 401", w.Code)
	}
	if w := e.do("GET", "/api/me", e.cookie, nil); w.Code != 200 {
		t.Fatalf("current session after enable: %d", w.Code)
	}
}

func TestNoNewTOTPKeyWhileTOTPIsOn(t *testing.T) {
	e := newTOTPEnv(t)
	e.enable(e.setup())
	path := filepath.Join(e.dir, totpKeyFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.svc.key = nil
	if _, err := e.svc.totpKey(); err == nil || !strings.Contains(err.Error(), "not creating a new key") {
		t.Fatalf("totpKey = %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a new key file was created")
	}
	if !strings.Contains(e.logs.String(), "totp key missing") {
		t.Errorf("reason not logged:\n%s", e.logs)
	}
	// With TOTP off, creating a key is fine.
	if _, err := e.svc.DisableTOTP(context.Background(), AdminUser); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.totpKey(); err != nil {
		t.Fatalf("key with TOTP off: %v", err)
	}
}

// Parallel wrong codes from many addresses: the attempt is counted before the
// code is checked, so exactly MaxTOTPFailures are judged and the rest are
// refused as locked (no extra guesses slip through).
func TestTOTPLockHoldsUnderParallelRequests(t *testing.T) {
	e := newTOTPEnv(t)
	e.enable(e.setup())
	const n = 40
	var wrong, locked, other atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(map[string]string{"username": "admin", "password": e.pw, "code": "111111"})
			r := httptest.NewRequest("POST", "https://portal.test/api/login", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(RequestHeader, "1")
			r.RemoteAddr = "198.51.100." + strconv.Itoa(i+1) + ":4000"
			w := httptest.NewRecorder()
			e.h.ServeHTTP(w, r)
			switch {
			case w.Code == http.StatusUnauthorized && strings.Contains(w.Body.String(), "bad_totp"):
				wrong.Add(1)
			case w.Code == http.StatusTooManyRequests && strings.Contains(w.Body.String(), "totp_locked"):
				locked.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if other.Load() != 0 || wrong.Load() != MaxTOTPFailures || locked.Load() != n-MaxTOTPFailures {
		t.Fatalf("wrong=%d locked=%d other=%d, want %d/%d/0", wrong.Load(), locked.Load(), other.Load(), MaxTOTPFailures, n-MaxTOTPFailures)
	}
}
