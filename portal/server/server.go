// Package server is the portal's HTTP server. TLS ends at the Traefik ingress.
package server

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Handler returns the portal's HTTP routes: /healthz plus whatever each
// register function adds (for example the login API).
func Handler(register ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)
	for _, r := range register {
		r(mux)
	}
	return securityHeaders(mux)
}

// defaultCSP is the Content-Security-Policy of every response: nothing may
// load or frame it (review KM-44 L14). The web app's index sets its own.
const defaultCSP = "default-src 'none'; frame-ancestors 'none'"

// securityHeaders sets headers every portal response should carry.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("Content-Security-Policy", defaultCSP)
		next.ServeHTTP(w, r)
	})
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// Public: no version or cluster details (anyone on the internet can call it).
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// New returns an HTTP server for addr. tlsConfig may be nil (plain HTTP behind
// the ingress); with one, start it with ListenAndServeTLS("", "").
func New(addr string, h http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           bodyDeadline(h),
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// The largest request headers are a session cookie and the agents'
		// Authorization header; 1 MiB (the default) only helps a flood.
		MaxHeaderBytes: MaxHeaderBytes,
	}
}

// MaxHeaderBytes bounds a request's headers (review-km-44 L12).
const MaxHeaderBytes = 64 << 10

// BodyReadTimeout is how long a client may take to send a request body.
var BodyReadTimeout = 30 * time.Second

// bodyDeadline bounds the time a request's body may take to arrive, so a
// client that sends its body slowly cannot hold a connection (and a
// login's bcrypt slot) forever (review-km-44 L12). http.Server.ReadTimeout
// would do this, but it also covers the whole life of WebSocket tunnels and
// followed logs, so the deadline is set per request, only for requests with
// a body and not for upgrades, and lifted as soon as the body is read.
func bodyDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		if rc.SetReadDeadline(time.Now().Add(BodyReadTimeout)) != nil {
			next.ServeHTTP(w, r) // the connection cannot take a deadline
			return
		}
		lift := func() { rc.SetReadDeadline(time.Time{}) }
		defer lift()
		r.Body = &liftingBody{ReadCloser: r.Body, lift: lift}
		next.ServeHTTP(w, r)
	})
}

// liftingBody lifts the read deadline once the body has been read.
type liftingBody struct {
	io.ReadCloser
	lift func()
	done bool
}

func (b *liftingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF && !b.done {
		b.done = true
		b.lift()
	}
	return n, err
}
