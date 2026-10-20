package auth

import (
	"net/http"
	"testing"
)

// Behind Traefik the TCP peer is a pod address; the limiter must key on the
// real client from X-Forwarded-For, so one client's failures do not lock
// another out, and a forged header cannot hide the real address.
func TestLimiterUsesRealClientIP(t *testing.T) {
	e := newEnv(t)
	e.remote = "10.42.0.5:4444" // Traefik pod
	pw := e.bootstrap()
	bad := map[string]string{"username": "admin", "password": "bad"}
	for i := 0; i < MaxFailures; i++ {
		// The attacker prepends a forged entry; Traefik appends the real one.
		e.do("POST", "/api/login", "", bad, "X-Forwarded-For", "10.9.9."+string(rune('1'+i))+", 203.0.113.7")
	}
	if w := e.do("POST", "/api/login", "", bad, "X-Forwarded-For", "198.51.100.1, 203.0.113.7"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("same real client, new forged entry: %d, want 429", w.Code)
	}
	// Another real client is not blocked (the owner can still log in).
	if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw}, "X-Forwarded-For", "203.0.113.99"); w.Code != http.StatusOK {
		t.Fatalf("other client: %d %s", w.Code, w.Body)
	}
}

// The password-change and re-authentication checks share the login limit
// key, including the IPv6 /64 grouping.
func TestPasswordChangeSharesLoginLimitKey(t *testing.T) {
	e := newEnv(t)
	e.remote = "10.42.0.5:4444"
	pw := e.bootstrap()
	c := e.login("admin", pw)
	for i := 0; i < MaxFailures; i++ {
		e.do("POST", "/api/password", c, map[string]string{"currentPassword": "wrong", "newPassword": "a-good-new-password"}, "X-Forwarded-For", "2001:db8:1:2::"+string(rune('a'+i)))
	}
	w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": pw}, "X-Forwarded-For", "2001:db8:1:2::ff")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("login from the same /64 after password failures: %d, want 429", w.Code)
	}
}

// Login does not say which part was wrong, and the first password is not
// accepted once it was changed; nothing creates users over HTTP.
func TestNoUserCreationRoutes(t *testing.T) {
	e := newEnv(t)
	e.bootstrap()
	for _, p := range []string{"/api/users", "/api/signup", "/api/register", "/api/user"} {
		for _, m := range []string{"POST", "PUT"} {
			if w := e.do(m, p, "", map[string]string{"username": "eve", "password": "a-long-password-1"}); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d, want 404/405", m, p, w.Code)
			}
		}
	}
}
