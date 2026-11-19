package brandimages

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// robots.txt is obeyed when a website is read for pictures: the group for MarketFlowBot when the
// site has one, else the group for every robot. The longest matching rule wins and Allow wins a
// tie, as most crawlers read it. A site without a readable robots.txt may be read.

const robotsAgent = "marketflowbot"

// userAgent is how MarketFlow names itself to the websites it reads.
const userAgent = "MarketFlowBot/1.0 (brand pictures for the business's own ads)"

type robotsRule struct {
	allow  bool
	prefix string
}

// parseRobots returns the rules that apply to MarketFlowBot.
func parseRobots(body string) []robotsRule {
	groups := map[string][]robotsRule{}
	var agents []string
	inRules := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if inRules {
				agents, inRules = nil, false
			}
			agents = append(agents, strings.ToLower(v))
		case "allow", "disallow":
			inRules = true
			if v == "" {
				continue // "Disallow:" with nothing allows everything
			}
			for _, a := range agents {
				groups[a] = append(groups[a], robotsRule{allow: k == "allow", prefix: v})
			}
		}
	}
	for a, rules := range groups {
		if a != "*" && strings.Contains(robotsAgent, a) {
			return rules
		}
	}
	return groups["*"]
}

// robotsAllowPath reports whether rules allow the path (with its query).
func robotsAllowPath(rules []robotsRule, p string) bool {
	best, allow := -1, true
	for _, r := range rules {
		if n := robotsMatch(r.prefix, p); n > best || (n == best && r.allow) {
			if n >= 0 {
				best, allow = n, r.allow
			}
		}
	}
	return allow
}

// robotsMatch returns the length of pattern when it matches the start of p ('*' is any run of
// characters, a final '$' ends the path), else -1.
func robotsMatch(pattern, p string) int {
	anchored := strings.HasSuffix(pattern, "$")
	pat := strings.TrimSuffix(pattern, "$")
	parts := strings.Split(pat, "*")
	pos := 0
	for i, part := range parts {
		if i == 0 {
			if !strings.HasPrefix(p, part) {
				return -1
			}
			pos = len(part)
			continue
		}
		j := strings.Index(p[pos:], part)
		if j < 0 {
			return -1
		}
		pos += j + len(part)
	}
	if anchored && pos != len(p) {
		return -1
	}
	return len(pattern)
}

// robots reads a site's robots.txt once and returns the check for its pages.
func (s *Service) robots(ctx context.Context, site string) func(page string) bool {
	allowAll := func(string) bool { return true }
	base, err := url.Parse(site)
	if err != nil {
		return allowAll
	}
	rctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base.Scheme+"://"+base.Host+"/robots.txt", nil)
	if err != nil {
		return allowAll
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.web.Do(req)
	if err != nil {
		return allowAll
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return allowAll
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	rules := parseRobots(string(b))
	return func(page string) bool {
		u, err := url.Parse(page)
		if err != nil {
			return false
		}
		p := u.EscapedPath()
		if p == "" {
			p = "/"
		}
		if u.RawQuery != "" {
			p += "?" + u.RawQuery
		}
		return robotsAllowPath(rules, p)
	}
}
