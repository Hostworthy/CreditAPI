// Package forwarded decides which client address, scheme and host a request
// came from. By default the connection itself is used and forwarding headers
// (X-Forwarded-For, -Proto, -Host) are ignored, because any client can send
// them. Requests that arrive from a configured trusted proxy (TRUSTED_PROXIES)
// may override them.
package forwarded

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Trust holds the proxies whose forwarding headers are believed. The zero
// value (or nil) trusts nobody.
type Trust struct {
	prefixes []netip.Prefix
}

// Parse reads a comma-separated list of addresses and CIDR ranges.
func Parse(list string) (*Trust, error) {
	t := &Trust{}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if p, err := netip.ParsePrefix(item); err == nil {
			t.prefixes = append(t.prefixes, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", item)
		}
		t.prefixes = append(t.prefixes, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return t, nil
}

func (t *Trust) trusted(a netip.Addr) bool {
	if t == nil || !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// fromProxy reports whether the request came straight from a trusted proxy.
func (t *Trust) fromProxy(r *http.Request) bool { return t.trusted(remoteAddr(r)) }

// ClientIP returns the client address: the connection's address, or behind
// a trusted proxy the right-most X-Forwarded-For entry that is not itself a
// trusted proxy.
func (t *Trust) ClientIP(r *http.Request) string {
	remote := remoteAddr(r)
	if !t.trusted(remote) {
		if remote.IsValid() {
			return remote.String()
		}
		return r.RemoteAddr
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !t.trusted(a) {
			return a.Unmap().String()
		}
	}
	return remote.String()
}

// HTTPS reports whether the client used HTTPS.
func (t *Trust) HTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return t.fromProxy(r) && strings.EqualFold(firstValue(r.Header.Get("X-Forwarded-Proto")), "https")
}

// Host returns the host the client asked for.
func (t *Trust) Host(r *http.Request) string {
	if t.fromProxy(r) {
		if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" {
			return h
		}
	}
	return r.Host
}

func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}

// LimitKey groups addresses for rate limiting: IPv6 clients usually own a
// whole /64, so it counts as one client.
func LimitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() || a.Is4In6() {
		return ip
	}
	p, _ := a.Prefix(64)
	return p.String()
}
