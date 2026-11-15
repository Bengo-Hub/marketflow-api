package marketingos

import (
	"context"
	"strings"
)

// SiteSummary is what a business's public home page says about itself: its title and meta
// description, nothing else. It goes through readSite, so it shares the public-only client
// (internal addresses refused after DNS, https only) and the day cache. A link without a scheme
// ("acme.co.ke") is read over https.
func SiteSummary(ctx context.Context, link string) (string, error) {
	link = strings.TrimSpace(link)
	if link != "" && !strings.Contains(link, "://") {
		link = "https://" + link
	}
	facts, err := readSite(ctx, link)
	if err != nil {
		return "", err
	}
	return facts.Summary, nil
}
