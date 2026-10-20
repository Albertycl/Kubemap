package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Every response has a CSP (review KM-44 L14).
func TestDefaultCSP(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("Content-Security-Policy"); got != defaultCSP {
		t.Fatalf("CSP %q", got)
	}
}
