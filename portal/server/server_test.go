package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Fatalf("body %v; want only status ok (no version or cluster details)", body)
	}

	rec = httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status %d, want 405", rec.Code)
	}
}

// review-km-44 L12: a request body that arrives too slowly is cut off; a
// body that arrived in time, a GET and a long answer are not limited.
func TestBodyReadDeadline(t *testing.T) {
	old := BodyReadTimeout
	BodyReadTimeout = 300 * time.Millisecond
	defer func() { BodyReadTimeout = old }()

	readErr := make(chan error, 4)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
		if err != nil {
			return
		}
		time.Sleep(3 * BodyReadTimeout) // a long answer after the body: a followed log, a slow agent call
		w.Write([]byte("done"))
	})
	srv := New("127.0.0.1:0", h, nil)
	if srv.MaxHeaderBytes != 64<<10 {
		t.Errorf("MaxHeaderBytes = %d", srv.MaxHeaderBytes)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	defer srv.Close()
	url := "http://" + l.Addr().String() + "/x"

	// Slow body: 5 of 100 bytes, then nothing.
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	fmt.Fprint(c, "POST /x HTTP/1.1\r\nHost: a\r\nContent-Length: 100\r\n\r\nhello")
	select {
	case err := <-readErr:
		if err == nil || time.Since(start) > 5*BodyReadTimeout {
			t.Fatalf("slow body: err %v after %v", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a slow body is not cut off")
	}

	// A whole body, then a long answer.
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "done" || <-readErr != nil {
		t.Fatalf("body then long answer: %d %q", resp.StatusCode, b)
	}

	// A GET (no body) with a long answer.
	resp, err = http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "done" {
		t.Fatalf("GET: %q", b)
	}
}

// The security headers: HSTS only when the browser came over https (TLS or
// X-Forwarded-Proto=https), the rest always.
func TestSecurityHeaders(t *testing.T) {
	get := func(proto string) http.Header {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, r)
		return rec.Header()
	}
	for _, proto := range []string{"", "http", "https"} {
		h := get(proto)
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Referrer-Policy") != "no-referrer" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("proto %q: headers %v", proto, h)
		}
		if got := h.Get("Strict-Transport-Security") != ""; got != (proto == "https") {
			t.Errorf("proto %q: HSTS present = %v", proto, got)
		}
	}
}
