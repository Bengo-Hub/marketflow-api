package publishing

import (
	"strings"
	"testing"
)

func TestTikTokPostInfoUsesTheChoice(t *testing.T) {
	ci := tiktokCreator{PrivacyLevelOptions: []string{"PUBLIC_TO_EVERYONE", "SELF_ONLY"}}
	p := Post{Caption: "Hi", AIGenerated: true, TikTok: &TikTokOptions{PrivacyLevel: "PUBLIC_TO_EVERYONE", AllowComment: true, BrandOrganic: true}}
	info, err := tiktokPostInfo(ci, p, "SELF_ONLY")
	if err != nil {
		t.Fatal(err)
	}
	if info["privacy_level"] != "PUBLIC_TO_EVERYONE" || info["disable_comment"] != false || info["disable_duet"] != true ||
		info["disable_stitch"] != true || info["is_aigc"] != true || info["brand_organic_toggle"] != true {
		t.Fatalf("post_info %v", info)
	}
	if _, ok := info["brand_content_toggle"]; ok {
		t.Fatal("brand_content_toggle sent although not chosen")
	}
}

func TestTikTokPostInfoCreatorSettingsWin(t *testing.T) {
	ci := tiktokCreator{PrivacyLevelOptions: []string{"SELF_ONLY"}, CommentDisabled: true}
	p := Post{TikTok: &TikTokOptions{PrivacyLevel: "SELF_ONLY", AllowComment: true, AllowDuet: true, AllowStitch: true}}
	info, err := tiktokPostInfo(ci, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if info["disable_comment"] != true || info["disable_duet"] != false {
		t.Fatalf("a creator who turned comments off keeps them off: %v", info)
	}
	p.TikTok.PrivacyLevel = "PUBLIC_TO_EVERYONE"
	if _, err := tiktokPostInfo(ci, p, ""); err == nil {
		t.Fatal("a privacy level the creator may not use must fail")
	}
}

func TestTikTokPostInfoWithoutChoiceKeepsDefaults(t *testing.T) {
	ci := tiktokCreator{PrivacyLevelOptions: []string{"SELF_ONLY"}, StitchDisabled: true}
	info, err := tiktokPostInfo(ci, Post{}, "SELF_ONLY")
	if err != nil {
		t.Fatal(err)
	}
	if info["privacy_level"] != "SELF_ONLY" || info["disable_comment"] != false || info["disable_stitch"] != true {
		t.Fatalf("defaults %v", info)
	}
}

func TestTikTokBrandedContentNotPrivate(t *testing.T) {
	ci := tiktokCreator{PrivacyLevelOptions: []string{"SELF_ONLY"}}
	if _, err := tiktokPostInfo(ci, Post{TikTok: &TikTokOptions{PrivacyLevel: "SELF_ONLY", BrandContent: true}}, ""); err == nil {
		t.Fatal("branded content with SELF_ONLY must fail")
	}
}

func TestCheckTikTokChoice(t *testing.T) {
	cases := []struct {
		name    string
		o       *TikTokOptions
		consent bool
		want    string
	}{
		{"no options", nil, true, "choose who can see"},
		{"empty privacy", &TikTokOptions{}, true, "choose who can see"},
		{"unknown privacy", &TikTokOptions{PrivacyLevel: "FRIENDS"}, true, "unknown TikTok privacy"},
		{"branded private", &TikTokOptions{PrivacyLevel: "SELF_ONLY", BrandContent: true}, true, "branded content cannot be private"},
		{"no consent", &TikTokOptions{PrivacyLevel: "SELF_ONLY"}, false, "Music Usage Confirmation"},
		{"complete", &TikTokOptions{PrivacyLevel: "FOLLOWER_OF_CREATOR"}, true, ""},
	}
	for _, c := range cases {
		got := checkTikTokChoice(c.o, c.consent)
		if (c.want == "" && got != "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
