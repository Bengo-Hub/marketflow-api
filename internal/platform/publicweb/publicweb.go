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
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return ErrNotPublic
	}
	return nil
}
