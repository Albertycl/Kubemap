package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"kubemap/portal/store"
)

func TestReauthenticate(t *testing.T) {
	e := newEnv(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct horse battery"), bcrypt.MinCost)
	if err := e.st.CreateUser(context.Background(), store.User{Username: "admin", PasswordHash: string(hash)}); err != nil {
		t.Fatal(err)
	}
	check := func(user, pw string) int {
		r := httptest.NewRequest("PUT", "/x", nil)
		r.RemoteAddr = "192.0.2.10:5555"
		if user != "" {
			r = r.WithContext(ContextWithUser(r.Context(), user))
		}
		w := httptest.NewRecorder()
		if e.svc.Reauthenticate(w, r, pw) {
			return http.StatusOK
		}
		return w.Code
	}
	if c := check("", "correct horse battery"); c != 401 {
		t.Fatalf("no user: %d", c)
	}
	if c := check("admin", "correct horse battery"); c != 200 {
		t.Fatalf("right password: %d", c)
	}
	// A success does not count against the limit; failures do, shared with login.
	for i := 0; i < 5; i++ {
		if c := check("admin", "wrong"); c != 403 {
			t.Fatalf("wrong password %d: %d", i, c)
		}
	}
	if c := check("admin", "correct horse battery"); c != 429 {
		t.Fatalf("after 5 failures: %d, want 429", c)
	}
	if w := e.do("POST", "/api/login", "", map[string]string{"username": "admin", "password": "correct horse battery"}); w.Code != 429 {
		t.Fatalf("login after 5 failed checks: %d, want 429", w.Code)
	}
	es, _ := e.st.ListAudit(context.Background(), 10)
	if len(es) == 0 || es[0].Action != "reauth" || es[0].Result != "failed" {
		t.Fatalf("audit %+v", es)
	}
}
