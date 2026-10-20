package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A password typed into the username field is not logged or audited
// (review KM-44 L8); a real account's name is.
func TestFailedLoginDoesNotLogUnknownNames(t *testing.T) {
	e := newEnv(t)
	e.bootstrap()
	secretish := "Tr0ub4dor&3-my-password"
	if w := e.do("POST", "/api/login", "", map[string]string{"username": secretish, "password": "x"}); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "wrong"}); w.Code != 401 {
		t.Fatalf("%d", w.Code)
	}
	es, _ := e.st.ListAudit(context.Background(), 10)
	var users []string
	for _, a := range es {
		users = append(users, a.User)
	}
	all := strings.Join(users, "|") + e.logs.String()
	if strings.Contains(all, secretish) || strings.Contains(all, "Tr0ub") {
		t.Fatalf("the typed name leaked:\n%s", all)
	}
	if !strings.Contains(all, "(unknown user, 23 characters)") || !strings.Contains(strings.Join(users, "|"), "admin") {
		t.Fatalf("audit users %v", users)
	}
}

func TestClientKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.10":            "192.0.2.10",
		"2001:db8:1:2:aaaa::1":  "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::99": "2001:db8:1:2::/64",
		"::ffff:192.0.2.10":     "::ffff:192.0.2.10",
		"not-an-ip":             "not-an-ip",
	} {
		if got := clientKey(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// Failures for one username slow the next tries down, from any address,
// but never block them (review KM-44 L13).
func TestBackoff(t *testing.T) {
	b := newBackoff()
	now := time.Now()
	for i := 0; i < backoffFree; i++ {
		if d := b.delay("admin", now); d != 0 {
			t.Fatalf("delay after %d failures: %v", i, d)
		}
		b.failed("admin", now)
	}
	if d := b.delay("admin", now); d != time.Second {
		t.Fatalf("after %d failures: %v", backoffFree, d)
	}
	for i := 0; i < 20; i++ {
		b.failed("admin", now)
	}
	if d := b.delay("admin", now); d != backoffMax {
		t.Fatalf("capped delay %v", d)
	}
	if d := b.delay("other", now); d != 0 {
		t.Fatalf("another name is slowed: %v", d)
	}
	if d := b.delay("admin", now.Add(FailureWindow)); d != 0 {
		t.Fatalf("after the window: %v", d)
	}
	b.succeeded("admin")
	if d := b.delay("admin", now); d != 0 {
		t.Fatalf("after a success: %v", d)
	}
}
