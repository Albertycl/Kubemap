package httpjson

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Cleanup(func() { _ = SetTrustedProxies(DefaultTrustedProxies) })
	cases := []struct {
		name, trust, peer, xff, want string
	}{
		{"direct, no header", DefaultTrustedProxies, "203.0.113.5:4000", "", "203.0.113.5"},
		{"trusted peer, one hop", DefaultTrustedProxies, "10.42.0.7:4000", "198.51.100.9", "198.51.100.9"},
		{"client-sent value is left of Traefik's", DefaultTrustedProxies, "10.42.0.7:4000", "1.2.3.4, 198.51.100.9", "198.51.100.9"},
		{"skips trusted hops", DefaultTrustedProxies, "10.42.0.7:4000", "198.51.100.9, 10.42.0.3", "198.51.100.9"},
		{"spoof from untrusted peer ignored", DefaultTrustedProxies, "203.0.113.5:4000", "1.2.3.4", "203.0.113.5"},
		{"trusted peer, no header", DefaultTrustedProxies, "10.42.0.7:4000", "", "10.42.0.7"},
		{"garbled header", DefaultTrustedProxies, "10.42.0.7:4000", "not-an-ip", "10.42.0.7"},
		{"all hops trusted", DefaultTrustedProxies, "10.42.0.7:4000", "10.1.1.1", "10.42.0.7"},
		{"empty trust list", "", "10.42.0.7:4000", "198.51.100.9", "10.42.0.7"},
		{"loopback trusted", DefaultTrustedProxies, "127.0.0.1:1", "198.51.100.9", "198.51.100.9"},
		{"ipv6 mapped peer", DefaultTrustedProxies, "[::ffff:10.0.0.1]:1", "2001:db8::1", "2001:db8::1"},
		{"single IP trust", "192.0.2.1", "192.0.2.1:1", "198.51.100.9", "198.51.100.9"},
	}
	for _, c := range cases {
		if err := SetTrustedProxies(c.trust); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := ClientIP(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestSetTrustedProxiesRejectsJunk(t *testing.T) {
	t.Cleanup(func() { _ = SetTrustedProxies(DefaultTrustedProxies) })
	if err := SetTrustedProxies("10.0.0.0/8, nope"); err == nil {
		t.Fatal("want error")
	}
}

func TestClientAddrShared(t *testing.T) {
	t.Cleanup(func() { _ = SetTrustedProxies(DefaultTrustedProxies) })
	cases := []struct {
		name, peer, xff string
		want            bool
	}{
		{"direct client", "203.0.113.5:4000", "", false},
		{"real client behind proxy", "10.42.0.7:4000", "198.51.100.9", false},
		{"garbled header", "10.42.0.7:4000", "junk", true},
		{"no header from proxy (SNAT)", "10.42.0.7:4000", "", true},
		{"only proxy hops", "10.42.0.7:4000", "10.1.1.1", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if _, got := ClientAddr(r); got != c.want {
			t.Errorf("%s: shared=%v, want %v", c.name, got, c.want)
		}
	}
}
