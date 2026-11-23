package forwarded

import (
	"net/http/httptest"
	"testing"
)

func TestTrust(t *testing.T) {
	tr, err := Parse(" 10.0.0.0/8, 192.168.1.5 ,::1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("nope"); err == nil {
		t.Fatal("invalid entry accepted")
	}
	cases := []struct {
		remote, xff, proto, host string
		wantIP, wantHost         string
		wantHTTPS                bool
	}{
		// Untrusted peer: headers ignored.
		{"203.0.113.9:1234", "1.2.3.4", "https", "evil.example", "203.0.113.9", "api.example", false},
		// Trusted proxy: right-most untrusted hop wins.
		{"10.1.2.3:80", "1.2.3.4, 198.51.100.7, 10.9.9.9", "https", "ai.example", "198.51.100.7", "ai.example", true},
		// Trusted proxy without headers: the proxy itself.
		{"192.168.1.5:80", "", "", "", "192.168.1.5", "api.example", false},
		// Garbage in the header stops the walk.
		{"[::1]:80", "not-an-ip", "", "", "::1", "api.example", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://api.example/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.proto != "" {
			r.Header.Set("X-Forwarded-Proto", c.proto)
		}
		if c.host != "" {
			r.Header.Set("X-Forwarded-Host", c.host)
		}
		if got := tr.ClientIP(r); got != c.wantIP {
			t.Errorf("%s: ip %q want %q", c.remote, got, c.wantIP)
		}
		if got := tr.Host(r); got != c.wantHost {
			t.Errorf("%s: host %q want %q", c.remote, got, c.wantHost)
		}
		if got := tr.HTTPS(r); got != c.wantHTTPS {
			t.Errorf("%s: https %v want %v", c.remote, got, c.wantHTTPS)
		}
	}
	var none *Trust
	r := httptest.NewRequest("GET", "http://x/", nil)
	r.RemoteAddr = "10.1.1.1:5"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if none.ClientIP(r) != "10.1.1.1" || none.HTTPS(r) {
		t.Fatal("nil trust must ignore headers")
	}
}

func TestLimitKey(t *testing.T) {
	if LimitKey("2001:db8:1:2:3:4:5:6") != LimitKey("2001:db8:1:2:ffff::1") {
		t.Fatal("same /64 should share a key")
	}
	if LimitKey("1.2.3.4") != "1.2.3.4" || LimitKey("junk") != "junk" {
		t.Fatal("IPv4 and junk are kept as is")
	}
}
