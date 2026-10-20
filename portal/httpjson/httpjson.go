// Package httpjson holds the small JSON helpers the portal API shares.
package httpjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// MaxBodyBytes bounds every JSON request body.
const MaxBodyBytes = 16 << 10

// Read decodes a JSON request body into v. It requires Content-Type
// application/json and rejects unknown fields. On failure it has already
// written the error response and returns false.
func Read(w http.ResponseWriter, r *http.Request, v any) bool {
	return ReadLimit(w, r, v, MaxBodyBytes)
}

// ReadLimit is Read with its own body limit, for the few APIs that take
// larger documents (the incident journal).
func ReadLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		Error(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("the request body is larger than %d KiB", limit>>10))
			return false
		}
		Error(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return false
	}
	return true
}

// Write sends v as JSON with the given status. Responses are never cached.
func Write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ErrorBody is the JSON of an error answer: {"error": code, "message": msg}.
// It is also used inside answers that report several parts.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Failure is an error that knows its answer: the HTTP status, the error
// code and the message for the UI.
type Failure struct {
	Status  int
	Code    string
	Message string
}

func (f *Failure) Error() string { return f.Message }

// Error sends {"error": code, "message": msg}.
func Error(w http.ResponseWriter, status int, code, msg string) {
	Write(w, status, ErrorBody{Error: code, Message: msg})
}

// DefaultTrustedProxies are the ranges whose X-Forwarded-For header is
// believed: loopback and the private ranges where cluster pods live.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"

var trusted atomic.Pointer[[]netip.Prefix]

func init() { _ = SetTrustedProxies(DefaultTrustedProxies) }

// SetTrustedProxies sets the proxies (comma-separated CIDRs or single IPs)
// whose X-Forwarded-For header ClientIP believes. Empty trusts none.
func SetTrustedProxies(list string) error {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return fmt.Errorf("trusted proxy %q: not an IP address or CIDR", f)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	trusted.Store(&out)
	return nil
}

func isTrusted(s string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range *trusted.Load() {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP is the address of the caller. It is the TCP peer, unless the
// peer is a trusted proxy (for example Traefik): then it is the right-most
// X-Forwarded-For entry that is not itself a trusted proxy. A header from
// any other peer is ignored, so a client cannot choose its own address.
func ClientIP(r *http.Request) string {
	ip, _ := ClientAddr(r)
	return ip
}

// ClientAddr is ClientIP plus shared: true when the address returned is
// itself a trusted proxy (a garbled or missing X-Forwarded-For behind a
// proxy, or a proxy/SNAT address that stands for many visitors). Failure
// limits keyed on such an address must be far looser, or one visitor could
// lock out everybody behind it.
func ClientAddr(r *http.Request) (ip string, shared bool) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !isTrusted(peer) {
		return peer, false
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		a, err := netip.ParseAddr(h)
		if err != nil {
			return peer, true // garbled header: do not guess
		}
		if !isTrusted(h) {
			return a.Unmap().String(), false
		}
	}
	return peer, true
}
