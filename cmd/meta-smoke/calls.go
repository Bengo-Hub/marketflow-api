package main

import "net/url"

// call is one read-only Graph request that exercises a permission, for Meta's App Review
// "Testing" counter (each permission needs a successful call within 30 days).
type call struct {
	Permission string
	Token      string // "user" (the long-lived user token) or "page" (the Page token)
	Path       string
	Query      url.Values
}

// smokeCalls lists one GET per permission. Nothing here writes: posting (pages_manage_posts) and
// replying or hiding (pages_manage_engagement) are tested from MarketFlow itself, with the owner.
func smokeCalls(pageID, igID, adAccountID string) []call {
	q := func(kv ...string) url.Values {
		v := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	out := []call{
		{"public_profile", "user", "/me", q("fields", "id,name")},
		{"pages_show_list", "user", "/me/accounts", q("fields", "id,name", "limit", "5")},
		{"business_management", "user", "/me/businesses", q("fields", "id,name", "limit", "5")},
	}
	if adAccountID != "" {
		out = append(out,
			call{"ads_read", "user", "/" + adAccountID + "/insights", q("fields", "impressions,spend", "date_preset", "last_30d")},
			call{"ads_management", "user", "/" + adAccountID + "/campaigns", q("fields", "id,name,status", "limit", "5")},
		)
	}
	if pageID != "" {
		out = append(out,
			call{"pages_read_engagement", "page", "/" + pageID, q("fields", "id,name,fan_count,followers_count")},
			call{"pages_read_user_content", "page", "/" + pageID + "/feed", q("fields", "id,message,comments.limit(3){message}", "limit", "3")},
			call{"pages_manage_metadata", "page", "/" + pageID + "/subscribed_apps", nil},
			call{"pages_manage_ads", "page", "/" + pageID + "/ads_posts", q("limit", "3")},
			call{"pages_messaging", "page", "/" + pageID + "/conversations", q("limit", "3")},
			call{"leads_retrieval", "page", "/" + pageID + "/leadgen_forms", q("fields", "id,name,status", "limit", "5")},
		)
	}
	if igID != "" && pageID != "" {
		out = append(out,
			call{"instagram_basic", "page", "/" + igID, q("fields", "id,username,media_count")},
			call{"instagram_manage_comments", "page", "/" + igID + "/media", q("fields", "id,comments_count,comments.limit(3){text}", "limit", "3")},
			call{"instagram_content_publish", "page", "/" + igID + "/content_publishing_limit", nil},
			call{"instagram_manage_messages", "page", "/" + pageID + "/conversations", q("platform", "instagram", "limit", "3")},
		)
	}
	return out
}
