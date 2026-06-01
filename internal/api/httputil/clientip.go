package httputil

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIP returns the client's source address per the trusted-proxy
// allowlist.
//
// When r.RemoteAddr matches any entry in trustedProxies, the leftmost
// X-Forwarded-For hop is honored; otherwise XFF is ignored and the
// function falls back to RemoteAddr. trustedProxies may be nil/empty —
// in that case XFF is always ignored, which is the safe default for
// deployments without a reverse proxy in front of the coordinator.
//
// Returns the zero netip.Addr when no parseable address is available.
//
// M6.2 (B2) introduced this helper to close the TODO in
// internal/ratelimit/bucket.go acknowledging that XFF was attacker-
// controlled when the coordinator was reachable directly.
func ClientIP(r *http.Request, trustedProxies []netip.Prefix) netip.Addr {
	remote := r.RemoteAddr
	if h, _, err := net.SplitHostPort(remote); err == nil {
		remote = h
	}
	remoteAddr, _ := netip.ParseAddr(remote)

	// No trusted proxies configured ⇒ XFF is untrusted ⇒ return RemoteAddr.
	if len(trustedProxies) == 0 {
		return remoteAddr
	}

	// Is RemoteAddr in any trusted prefix? Only then is XFF honored.
	trusted := false
	if remoteAddr.IsValid() {
		for _, p := range trustedProxies {
			if p.Contains(remoteAddr) {
				trusted = true
				break
			}
		}
	}
	if !trusted {
		return remoteAddr
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteAddr
	}
	first := strings.TrimSpace(strings.Split(xff, ",")[0])
	if ip, err := netip.ParseAddr(first); err == nil {
		return ip
	}
	return remoteAddr
}

// ClientIPString is a convenience wrapper that returns a string key (for
// rate-limit map keying). Returns "" when no parseable address is
// available.
func ClientIPString(r *http.Request, trustedProxies []netip.Prefix) string {
	if ip := ClientIP(r, trustedProxies); ip.IsValid() {
		return ip.String()
	}
	return ""
}

// ParseTrustedProxies parses a comma-separated list of IPs or CIDR
// prefixes into []netip.Prefix. Bare IPs become /32 (IPv4) or /128
// (IPv6) prefixes. Empty input returns nil (caller treats this as
// "no proxies trusted, XFF ignored"). Invalid entries cause an error
// so misconfiguration is loud at startup.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, raw := range strings.Split(s, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if strings.Contains(raw, "/") {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, p)
			continue
		}
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, err
		}
		bits := 32
		if addr.Is6() && !addr.Is4In6() {
			bits = 128
		}
		out = append(out, netip.PrefixFrom(addr, bits))
	}
	return out, nil
}
