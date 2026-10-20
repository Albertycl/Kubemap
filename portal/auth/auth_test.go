package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"kubemap/portal/server"
	"kubemap/portal/store"
)

type env struct {
	t    *testing.T
	svc  *Service
	st   *store.SQLite
	h    http.Handler
	dir  string
	logs *bytes.Buffer
	now  time.Time
	// remote is the TCP peer of do's requests (default 192.0.2.10:5555).
	remote string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &bytes.Buffer{}
	svc, err := New(st, slog.New(slog.NewTextHandler(logs, nil)), dir)
	if err != nil {
		t.Fatal(err)
	}
	svc.WithBcryptCost(bcrypt.MinCost)
	e := &env{t: t, svc: svc, st: st, dir: dir, logs: logs, now: time.Now()}
	svc.now = func() time.Time { return e.now }
	protected := func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters", svc.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u, _ := UserFromContext(r.Context())
			io.WriteString(w, "clusters for "+u)
		})))
	}
	e.h = server.Handler(svc.Routes, protected)
	return e
}

// do sends a request; cookie may be empty. It returns the recorder.
func (e *env) do(method, path, cookie string, body any, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "https://portal.test"+path, rd)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = "192.0.2.10:5555"
	if e.remote != "" {
		r.RemoteAddr = e.remote
	}
	r.Header.Set(RequestHeader, "1") // as the web app sends it; hdr may clear it
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("no session cookie in response (status %d, body %s)", w.Code, w.Body)
	return nil
}

func (e *env) bootstrap() string {
	e.t.Helper()
	created, err := e.svc.Bootstrap(context.Background())
	if err != nil || !created {
		e.t.Fatalf("Bootstrap = %v, %v", created, err)
	}
	b, err := os.ReadFile(e.svc.InitialPasswordPath())
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func (e *env) login(user, pw string) string {
	e.t.Helper()
	w := e.do("POST", "/api/login", "", map[string]string{"username": user, "password": pw})
	if w.Code != http.StatusOK {
		e.t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	return sessionCookie(e.t, w).Value
}

func TestBootstrapCreatesAdminOnce(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	if len(pw) < 20 {
		t.Fatalf("initial password too short: %d chars", len(pw))
	}
	if st, _ := os.Stat(e.svc.InitialPasswordPath()); st.Mode().Perm() != 0o600 {
		t.Fatalf("initial password file mode %v, want 0600", st.Mode().Perm())
	}
	u, err := e.st.GetUser(context.Background(), AdminUser)
	if err != nil || !u.MustChangePassword || !strings.HasPrefix(u.PasswordHash, "$2") || strings.Contains(u.PasswordHash, pw) {
		t.Fatalf("admin user = %+v, %v; want bcrypt hash and must-change", u, err)
	}
	if created, err := e.svc.Bootstrap(context.Background()); created || err != nil {
		t.Fatalf("second Bootstrap = %v, %v; want false, nil", created, err)
	}
}

func TestLoginFlowAndMustChange(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()

	// No cookie: 401.
	if w := e.do("GET", "/api/me", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("me without login: %d", w.Code)
	}
	w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw})
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c := sessionCookie(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie flags: %+v", c)
	}
	if !strings.Contains(w.Body.String(), `"mustChangePassword":true`) {
		t.Fatalf("login body %s; want mustChangePassword true", w.Body)
	}
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}

	// Must change first: other API is blocked, /api/me works.
	if w := e.do("GET", "/api/clusters", c.Value, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "password_change_required") {
		t.Fatalf("protected route before password change: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/me", c.Value, nil); w.Code != http.StatusOK {
		t.Fatalf("me: %d", w.Code)
	}

	// Weak or same password is refused; wrong current password too.
	if w := e.do("POST", "/api/password", c.Value, map[string]string{"currentPassword": pw, "newPassword": "short"}); w.Code != http.StatusBadRequest {
		t.Fatalf("weak password: %d", w.Code)
	}
	if w := e.do("POST", "/api/password", c.Value, map[string]string{"currentPassword": pw, "newPassword": strings.Repeat("x", 73)}); w.Code != http.StatusBadRequest {
		t.Fatalf("73-byte password: %d", w.Code)
	}
	if w := e.do("POST", "/api/password", c.Value, map[string]string{"currentPassword": pw, "newPassword": pw}); w.Code != http.StatusBadRequest {
		t.Fatalf("same password: %d", w.Code)
	}
	if w := e.do("POST", "/api/password", c.Value, map[string]string{"currentPassword": "wrong", "newPassword": "a-good-new-password"}); w.Code != http.StatusForbidden {
		t.Fatalf("wrong current password: %d", w.Code)
	}

	// Change it: the old session ends, a new cookie is issued, the file goes.
	w = e.do("POST", "/api/password", c.Value, map[string]string{"currentPassword": pw, "newPassword": "a-good-new-password"})
	if w.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", w.Code, w.Body)
	}
	c2 := sessionCookie(t, w)
	if w := e.do("GET", "/api/me", c.Value, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session after password change: %d", w.Code)
	}
	if w := e.do("GET", "/api/clusters", c2.Value, nil); w.Code != http.StatusOK || w.Body.String() != "clusters for admin" {
		t.Fatalf("protected route after change: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(e.svc.InitialPasswordPath()); !os.IsNotExist(err) {
		t.Fatalf("initial password file still exists: %v", err)
	}
	// The old password no longer works; the new one does.
	if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw}); w.Code != http.StatusUnauthorized {
		t.Fatalf("login with old password: %d", w.Code)
	}
	e.login("admin", "a-good-new-password")

	// Logout ends the session.
	if w := e.do("POST", "/api/logout", c2.Value, nil); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := e.do("GET", "/api/clusters", c2.Value, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", w.Code)
	}
}

func TestWrongUserAndPasswordLookTheSame(t *testing.T) {
	e := newEnv(t)
	e.bootstrap()
	a := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "nope"})
	b := e.do("POST", "/api/login", "", map[string]string{"username": "nobody", "password": "nope"})
	if a.Code != http.StatusUnauthorized || a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Fatalf("responses differ: %d %s / %d %s", a.Code, a.Body, b.Code, b.Body)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	for i := 0; i < MaxFailures; i++ {
		if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "bad"}); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	// Locked, even with the right password.
	w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d failures: %d, Retry-After %q", MaxFailures, w.Code, w.Header().Get("Retry-After"))
	}
	e.now = e.now.Add(FailureWindow)
	e.login("admin", pw)
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	c := e.login("admin", pw)
	// Store uses the real clock; move the session's expiry into the past
	// by extending it to "now" as seen by the store.
	if err := e.st.ExtendSession(context.Background(), c, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	w := e.do("GET", "/api/me", c, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: %d", w.Code)
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Name == CookieName && ck.MaxAge >= 0 {
			t.Fatal("expired session cookie not cleared")
		}
	}
}

func TestSessionExpiryCappedAtMaxAge(t *testing.T) {
	e := newEnv(t)
	created := e.now
	e.now = created.Add(MaxSessionAge - time.Hour)
	if got := e.svc.sessionExpiry(created); !got.Equal(created.Add(MaxSessionAge)) {
		t.Fatalf("expiry %v, want capped at %v", got, created.Add(MaxSessionAge))
	}
	e.now = created.Add(time.Hour)
	if got := e.svc.sessionExpiry(created); !got.Equal(e.now.Add(IdleTimeout)) {
		t.Fatalf("expiry %v, want now+IdleTimeout", got)
	}
}

func TestCrossOriginAndContentType(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	body := map[string]string{"username": "admin", "password": pw}
	if w := e.do("POST", "/api/login", "", body, "Origin", "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d", w.Code)
	}
	if w := e.do("POST", "/api/login", "", body, "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site cross-site: %d", w.Code)
	}
	if w := e.do("POST", "/api/login", "", body, "Origin", "https://portal.test"); w.Code != http.StatusOK {
		t.Fatalf("same-origin login: %d %s", w.Code, w.Body)
	}
	// A form can post text/plain without a preflight: refused before the
	// body is read (review KM-44 L15).
	if w := e.do("POST", "/api/login", "", body, "Content-Type", "text/plain", RequestHeader, ""); w.Code != http.StatusForbidden {
		t.Fatalf("text/plain login from a form: %d", w.Code)
	}
	if w := e.do("POST", "/api/login", "", body, "Content-Type", "text/plain"); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain login from the app: %d", w.Code)
	}
	c := e.login("admin", pw)
	if w := e.do("POST", "/api/logout", c, nil, "Origin", "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin logout: %d", w.Code)
	}
	// A body-less write without the app's header, even with no Origin (a
	// plain form or img tag in an old browser): refused; with it: fine.
	if w := e.do("POST", "/api/logout", c, nil, RequestHeader, ""); w.Code != http.StatusForbidden {
		t.Fatalf("body-less logout without the header: %d", w.Code)
	}
	if w := e.do("POST", "/api/logout", c, nil); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("logout with the header: %d %s", w.Code, w.Body)
	}
}

func TestPasswordsNeverLoggedOrAudited(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "wrong-" + pw})
	c := e.login("admin", pw)
	e.do("POST", "/api/password", c, map[string]string{"currentPassword": pw, "newPassword": "a-good-new-password"})
	if strings.Contains(e.logs.String(), pw) || strings.Contains(e.logs.String(), "a-good-new-password") {
		t.Fatalf("password in logs:\n%s", e.logs)
	}
	files, _ := filepath.Glob(filepath.Join(e.dir, "portal.db*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte(pw)) || bytes.Contains(b, []byte("a-good-new-password")) {
			t.Fatalf("plaintext password in %s", filepath.Base(f))
		}
	}
	if !strings.Contains(e.logs.String(), "login failed") {
		t.Fatal("failed login not logged")
	}
}

// review-km-44 L8: a password typed into the username field must not end
// up in the log or the audit log; a real user name still does.
func TestLoginNameIsNotLoggedUnlessAUser(t *testing.T) {
	e := newEnv(t)
	e.bootstrap()
	secret := "MySecretPassw0rd!"
	e.do("POST", "/api/login", "", map[string]string{"username": secret, "password": "x"})
	e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "wrong-password-1"})
	if strings.Contains(e.logs.String(), secret) {
		t.Fatalf("unknown user name logged:\n%s", e.logs)
	}
	if !strings.Contains(e.logs.String(), "(unknown user, 17 characters)") || !strings.Contains(e.logs.String(), "username=admin") {
		t.Errorf("log lines:\n%s", e.logs)
	}
	files, _ := filepath.Glob(filepath.Join(e.dir, "portal.db*"))
	for _, f := range files {
		if b, _ := os.ReadFile(f); bytes.Contains(b, []byte(secret)) {
			t.Fatalf("unknown user name stored in %s", filepath.Base(f))
		}
	}
}

func TestResetAdminPassword(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	c := e.login("admin", pw)
	npw, err := e.svc.ResetAdminPassword(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/me", c, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("session survived reset: %d", w.Code)
	}
	w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": npw})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"mustChangePassword":true`) {
		t.Fatalf("login after reset: %d %s", w.Code, w.Body)
	}
}

// TestParallelLoginsCannotBypassLockout is the regression test for review
// B1: 40 parallel wrong logins from one address may check at most
// MaxFailures passwords; the rest are refused before any bcrypt work.
func TestParallelLoginsCannotBypassLockout(t *testing.T) {
	e := newEnv(t)
	e.bootstrap()
	var mu sync.Mutex
	calls := 0
	e.svc.compare = func(hash, pw []byte) error {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // like bcrypt: slow enough for requests to overlap
		return bcrypt.ErrMismatchedHashAndPassword
	}
	const n = 40
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "wrong"}).Code
		}()
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if calls > MaxFailures {
		t.Fatalf("%d password checks ran, want at most %d", calls, MaxFailures)
	}
	if count[http.StatusUnauthorized] != MaxFailures || count[http.StatusTooManyRequests] != n-MaxFailures {
		t.Fatalf("status codes %v, want %d x 401 and %d x 429", count, MaxFailures, n-MaxFailures)
	}
}

func TestBusyWhenHashSlotsFull(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	old := hashWait
	hashWait = 50 * time.Millisecond
	t.Cleanup(func() { hashWait = old })
	for i := 0; i < MaxConcurrentHashes; i++ {
		e.svc.hashSlots <- struct{}{}
	}
	w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("all slots busy: %d", w.Code)
	}
	for i := 0; i < MaxConcurrentHashes; i++ {
		<-e.svc.hashSlots
	}
	// The refused attempt was refunded: MaxFailures wrong tries still get 401.
	for i := 0; i < MaxFailures; i++ {
		if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "bad"}); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d after a busy refusal: %d", i, w.Code)
		}
	}
}

func TestLoginEndsOldSession(t *testing.T) {
	e := newEnv(t)
	pw := e.bootstrap()
	old := e.login("admin", pw)
	w := e.do("POST", "/api/login", old, map[string]string{"username": "admin", "password": pw})
	if w.Code != http.StatusOK {
		t.Fatalf("second login: %d", w.Code)
	}
	if w := e.do("GET", "/api/me", old, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("old session after a new login: %d", w.Code)
	}
}
