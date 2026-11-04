package links

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// errBlocked is a connection refused because its address isn't public;
// errPrivateRedirect a redirect to a link that looks private or one-time.
var (
	errBlocked         = errors.New("not a public address")
	errPrivateRedirect = errors.New("redirected to a private or one-time link")
)

// allowLoopback lets this package's own tests reach their local server.
var allowLoopback bool

// blocked are the addresses no link may reach, beyond those that aren't
// global unicast: private, shared, loopback, link-local (GCP's metadata
// server among them), documentation, benchmarking and reserved ranges, and
// IPv6 ranges that can carry an IPv4 address (NAT64, 6to4, Teredo).
var blocked = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48",
		"100::/64", "2001::/32", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// public reports whether ip is an address on the public internet.
func public(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1
	if !ip.IsGlobalUnicast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// guard refuses a connection to an address that isn't public. It runs on
// the address actually dialled, after DNS and for every attempt and
// redirect, so a name pointing at a private address, or DNS that changes
// its answer, can't get past it.
func guard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlocked
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errBlocked
	}
	if allowLoopback && ip.Unmap().IsLoopback() {
		return nil
	}
	if !public(ip) {
		return errBlocked
	}
	return nil
}

// newClient is the HTTP client links are fetched with: no proxy, so the
// address guarded is the one reached; TLS 1.2 or later, since many sites
// have no 1.3; at most maxRedirects redirects, each to http or https and
// none to a link that looks private or one-time.
func newClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: guard}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirected away from the web")
			}
			if private(req.URL) { // a short link to someone's reset link, say
				return errPrivateRedirect
			}
			return nil
		},
	}
}
