package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies liest eine durch Komma getrennte Liste aus IP-Adressen
// und Netzen, z. B. "10.42.0.0/16,192.168.0.5".
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("ungültige proxy-adresse %q", part)
			}
			out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("ungültiges proxy-netz %q", part)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// WithTrustedProxies legt fest, welchen Reverse-Proxies (z. B. Traefik in k3s)
// X-Forwarded-For und X-Forwarded-Proto geglaubt wird. Ohne Eintrag werden die
// Header ignoriert, weil jeder Client sie setzen kann.
func (s *Server) WithTrustedProxies(p []netip.Prefix) *Server {
	s.trustedProxies = p
	return s
}

func (s *Server) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range s.trustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

type httpsKey struct{}

// isHTTPS meldet, ob der Browser per HTTPS verbunden ist – direkt oder über
// einen vertrauenswürdigen Proxy mit TLS-Terminierung.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	v, _ := r.Context().Value(httpsKey{}).(bool)
	return v
}

// realIP ersetzt bei Anfragen von vertrauenswürdigen Proxies RemoteAddr durch
// die Client-Adresse aus X-Forwarded-For. Gewählt wird die rechteste Adresse,
// die kein vertrauenswürdiger Proxy ist: Weiter links stehende Einträge kann
// der Client selbst gefälscht haben.
func (s *Server) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if len(s.trustedProxies) == 0 || err != nil || !s.trusted(peer.Addr()) {
			next.ServeHTTP(w, r)
			return
		}
		var hops []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			hops = append(hops, strings.Split(v, ",")...)
		}
		for i := len(hops) - 1; i >= 0; i-- {
			a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				break // unlesbarer Eintrag: nicht weiter nach links gehen
			}
			if !s.trusted(a) {
				r = r.Clone(r.Context())
				r.RemoteAddr = netip.AddrPortFrom(a.Unmap(), 0).String()
				break
			}
		}
		if protos := r.Header.Values("X-Forwarded-Proto"); len(protos) > 0 &&
			strings.EqualFold(strings.TrimSpace(protos[len(protos)-1]), "https") {
			r = r.WithContext(context.WithValue(r.Context(), httpsKey{}, true))
		}
		next.ServeHTTP(w, r)
	})
}
