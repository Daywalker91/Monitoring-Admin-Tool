package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealIP(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.42.0.0/16, 192.168.0.5")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustedProxies("10.0.0.0/33"); err == nil {
		t.Error("ungültiges netz akzeptiert")
	}

	cases := []struct {
		name, remote, xff, proto string
		trusted                  bool
		wantIP                   string
		wantHTTPS                bool
	}{
		{"ohne proxy-liste", "10.42.0.7:5000", "1.2.3.4", "https", false, "10.42.0.7:5000", false},
		{"fremder absender", "8.8.8.8:5000", "1.2.3.4", "https", true, "8.8.8.8:5000", false},
		{"traefik", "10.42.0.7:5000", "192.168.0.220", "https", true, "192.168.0.220:0", true},
		{"gefälschter eintrag links", "10.42.0.7:5000", "6.6.6.6, 192.168.0.220", "http", true, "192.168.0.220:0", false},
		{"kette von proxies", "10.42.0.7:5000", "192.168.0.220, 192.168.0.5", "", true, "192.168.0.220:0", false},
		{"unlesbar", "10.42.0.7:5000", "kaputt", "", true, "10.42.0.7:5000", false},
	}
	for _, c := range cases {
		s := &Server{}
		if c.trusted {
			s.WithTrustedProxies(trusted)
		}
		var gotIP string
		var gotHTTPS bool
		h := s.realIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			gotIP, gotHTTPS = r.RemoteAddr, isHTTPS(r)
		}))
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		req.Header.Set("X-Forwarded-For", c.xff)
		if c.proto != "" {
			req.Header.Set("X-Forwarded-Proto", c.proto)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if gotIP != c.wantIP || gotHTTPS != c.wantHTTPS {
			t.Errorf("%s: ip=%s https=%v, erwartet %s %v", c.name, gotIP, gotHTTPS, c.wantIP, c.wantHTTPS)
		}
	}
}
