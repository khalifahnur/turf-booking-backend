// Package middleware holds the HTTP guard rails that sit in front of every
// handler. None of them wrap the ResponseWriter, so WebSocket upgrades (which
// need http.Hijacker) keep working.
package middleware

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// AutoProxyHops asks ClientIP to work out the client from a private peer.
const AutoProxyHops = -1

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isInternal(a netip.Addr) bool {
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || cgnat.Contains(a)
}

// ClientIP returns the address to rate-limit a request by.
//
// X-Forwarded-For is client-controlled except for what our own proxies append,
// so it is only believed from the right-hand side:
//   - hops == 0: no proxy, use the TCP peer.
//   - hops  > 0: exactly that many trusted proxies sit in front of us.
//   - AutoProxyHops: if the TCP peer is a private/loopback address (a platform
//     load balancer or local nginx) skip internal hops from the right; if the
//     peer is a public address nothing in the header is trusted.
func ClientIP(r *http.Request, hops int) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, perr := netip.ParseAddr(host)
	if hops == 0 || (hops == AutoProxyHops && (perr != nil || !isInternal(peer.Unmap()))) {
		return host
	}

	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				chain = append(chain, p)
			}
		}
	}
	if len(chain) == 0 {
		return host
	}

	if hops > 0 {
		i := len(chain) - hops
		if i < 0 {
			i = 0
		}
		return normalizeIP(chain[i], host)
	}

	for i := len(chain) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(chain[i])
		if err != nil {
			return host // malformed entry: don't guess
		}
		if !isInternal(a.Unmap()) {
			return a.Unmap().String()
		}
	}
	return normalizeIP(chain[0], host)
}

func normalizeIP(s, fallback string) string {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return fallback
	}
	return a.Unmap().String()
}
