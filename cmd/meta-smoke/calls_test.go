package main

import "testing"

// Every read-tested permission of the App Review list gets a call; the two write permissions are
// tested from MarketFlow with the owner, never from this command.
func TestSmokeCallsCoverPermissions(t *testing.T) {
	want := []string{"public_profile", "pages_show_list", "business_management", "ads_read", "ads_management",
		"pages_read_engagement", "pages_read_user_content", "pages_manage_metadata", "pages_manage_ads",
		"pages_messaging", "leads_retrieval", "instagram_basic", "instagram_manage_comments",
		"instagram_content_publish", "instagram_manage_messages"}
	got := map[string]bool{}
	for _, c := range smokeCalls("1", "2", "act_3") {
		got[c.Permission] = true
		if c.Token != "user" && c.Token != "page" {
			t.Errorf("%s: token %q", c.Permission, c.Token)
		}
	}
	for _, p := range want {
		if !got[p] {
			t.Errorf("no call for %s", p)
		}
	}
	for _, p := range []string{"pages_manage_posts", "pages_manage_engagement"} {
		if got[p] {
			t.Errorf("%s writes; it must not be in the smoke run", p)
		}
	}
	if n := len(smokeCalls("", "", "")); n != 3 {
		t.Errorf("with no assets only the user calls run, got %d", n)
	}
}
