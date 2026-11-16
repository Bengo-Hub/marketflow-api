// Package publicweb is an HTTP client for fetching a business's own public web pages and files.
// It connects only to public addresses: loopback, private, link-local and other internal ranges are
// refused at connect time (after DNS, so a name cannot point it inside the cluster), and every
// redirect must stay on https.
package publicweb

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrNotPublic is a connection or redirect to an address that is not public.
var ErrNotPublic = errors.New("address is not public")

// Client returns a client that only reaches public https addresses, with the given total timeout
// and at most maxRedirects redirects.
func Client(timeout time.Duration, maxRedirects int) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 4 * time.Second, Control: PublicOnly}).DialContext,
			TLSHandshakeTimeout:   4 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			MaxIdleConns:          10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects || req.URL.Scheme != "https" {
				return ErrNotPublic
			}
			return nil
		},
	}
}

// PublicOnly is a net.Dialer Control that refuses every address that is not a public unicast one.
func PublicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !IsPublic(net.ParseIP(host)) {
		return ErrNotPublic
	}
	return nil
}

// notPublic are ranges Go's IP checks count as global unicast but that are not reachable public
// hosts: carrier-grade NAT, benchmarking, IETF protocol assignments, documentation, reserved,
// "this network", NAT64 (which can reach IPv4 inside) and IPv6 unique local and documentation.
var notPublic = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"::/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32", "fc00::/7",
	} {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}()

// IsPublic reports whether ip is a public unicast address a fetch may connect to.
func IsPublic(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, n := range notPublic {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
