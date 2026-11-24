package worker

import (
	"net/url"
	"strings"
)

// sweepSites returns the websites one tenant's testimonial sweep reads: the website saved in
// MarketFlow's socials and the one on the tenant's SSO account (auth-api owns it), each once.
// Two links to the same host (www or not, http or https, a trailing path) count as one site.
func sweepSites(links ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range links {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !strings.Contains(l, "://") {
			l = "https://" + l
		}
		u, err := url.Parse(l)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		if seen[host] {
			continue
		}
		seen[host] = true
		if u.Scheme == "http" {
			u.Scheme = "https" // the importer reads https only
		}
		out = append(out, u.String())
	}
	return out
}
