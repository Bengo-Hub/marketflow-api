// Package publicweb is an HTTP client for fetching a business's own public web pages and files.
// It connects only to public addresses on https: the rule is the shared httpware/netguard guard
// (checked at connect time, after DNS, and on every redirect), which marketflow-ai uses too.
// This package keeps the names marketflow-api's callers use.
package publicweb

import (
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/Bengo-Hub/httpware/netguard"
)

// ErrNotPublic is a connection or redirect to an address that is not public.
var ErrNotPublic = netguard.ErrBlocked

// Client returns a client that only reaches public https addresses, with the given total timeout
// and at most maxRedirects redirects.
func Client(timeout time.Duration, maxRedirects int) *http.Client {
	if maxRedirects <= 0 {
		maxRedirects = -1 // netguard: follow none
	}
	return netguard.Client(netguard.Options{Timeout: timeout, MaxRedirects: maxRedirects})
}

// PublicOnly is a net.Dialer Control that refuses every address that is not a public unicast
// one on port 443.
func PublicOnly(network, address string, c syscall.RawConn) error {
	return netguard.Options{}.Control(network, address, c)
}

// IsPublic reports whether ip is a public unicast address a fetch may connect to.
func IsPublic(ip net.IP) bool { return netguard.IsPublicIP(ip) }
