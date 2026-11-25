package adsetup

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/integrations/google"
)

// Text limits of a Performance Max asset group, and how many rows the fix offers.
const (
	maxHeadline = 30
	maxLongText = 90
	textRows    = 3
)

// materialKind is one kind of Performance Max ad material. Want is what Google advises for a strong
// group, Max what it accepts.
type materialKind struct {
	Field, Name string
	Want, Max   int
	Limit       int // characters, 0 for pictures and videos
}

var materialKinds = []materialKind{
	{"HEADLINE", "headline", 5, 15, maxHeadline},
	{"LONG_HEADLINE", "long headline", 2, 5, maxLongText},
	{"DESCRIPTION", "description", 4, 5, maxLongText},
	{"MARKETING_IMAGE", "landscape picture", 2, 20, 0},
	{"SQUARE_MARKETING_IMAGE", "square picture", 2, 20, 0},
	{"PORTRAIT_MARKETING_IMAGE", "tall picture", 1, 20, 0},
	{"YOUTUBE_VIDEO", "video", 1, 5, 0},
}

var youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youtubeIDOf reads a YouTube id from a link or a bare id.
func youtubeIDOf(s string) string {
	s = strings.TrimSpace(s)
	if youtubeID.MatchString(s) {
		return s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	switch host := strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), "m."); host {
	case "youtu.be", "youtube.com":
	default:
		return ""
	}
	if v := u.Query().Get("v"); youtubeID.MatchString(v) {
		return v
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if v := parts[len(parts)-1]; youtubeID.MatchString(v) {
		return v
	}
	return ""
}

// materialItems is one check per Performance Max asset group. The texts proposed are the business's
// own service names and descriptions; the owner edits them before anything is sent.
func (in googleInput) materialItems(groups []google.AssetGroup, p Profile, err error) []Item {
	l := in.Cfg.link("google_assets", "Open assets", in.CustomerID)
	if err != nil {
		return []Item{unread("google.material", "Ad material in Performance Max", "Google", err, l)}
	}
	var out []Item
	for _, g := range groups {
		it := Item{Key: "google.material:" + g.ID, Title: "Ad material in \"" + g.Name + "\"", Link: l}
		low := map[string]int{}
		for _, k := range materialKinds {
			if n := g.Count[k.Field]; n < k.Want {
				low[k.Field] = k.Want - n
				it.Points = append(it.Points, fmt.Sprintf("%s: %d of %d", strings.ToUpper(k.Name[:1])+k.Name[1:], n, k.Want))
			}
		}
		if len(low) == 0 {
			it.Status, it.Link, it.Detail = StatusOK, nil, "This ad group has enough texts, pictures and videos."
			out = append(out, it)
			continue
		}
		it.Status = StatusWarning
		it.Detail = "Google mixes your texts, pictures and videos into ads. More of them means more places your ad can appear."
		if g.Strength != "" && g.Strength != "PENDING" && g.Strength != "UNSPECIFIED" && g.Strength != "UNKNOWN" {
			it.Detail += " Google rates this ad group " + strings.ToLower(words(g.Strength)) + "."
		}
		it.Fix = in.materialFix(g, low, p)
		if it.Fix == nil {
			it.Steps = []string{"Open this campaign in Google Ads and go to Asset groups.", "Press Edit on the ad group named here.", "Add the pictures listed above from your brand library and save."}
		} else if low["MARKETING_IMAGE"]+low["SQUARE_MARKETING_IMAGE"]+low["PORTRAIT_MARKETING_IMAGE"] > 0 {
			it.Detail += " Add pictures in Google Ads (Asset groups, Edit); MarketFlow adds the texts and the video."
		}
		out = append(out, it)
	}
	return out
}

func (in googleInput) materialFix(g google.AssetGroup, low map[string]int, p Profile) *Fix {
	fx := &Fix{Label: "Add texts and video", Changes: []string{
		"Add the texts and the video below to the ad group \"" + g.Name + "\". Nothing is removed.",
		"They must be true: Google and the law treat them as claims. Google reviews each one.",
		"Leave a row empty to skip it. Budgets and bids do not change.",
	}}
	for _, k := range materialKinds {
		if low[k.Field] == 0 {
			continue
		}
		if k.Field == "YOUTUBE_VIDEO" {
			v := ""
			for _, id := range p.Videos {
				if !g.Videos[id] {
					v = "https://youtu.be/" + id
					break
				}
			}
			fx.Fields = append(fx.Fields, Field{Key: "video_1", Label: "YouTube video link", Value: v, Max: 200})
			continue
		}
		if k.Limit == 0 {
			continue
		}
		texts := materialTexts(k, p, g.Texts)
		for i := 0; i < min(low[k.Field], textRows); i++ {
			v := ""
			if i < len(texts) {
				v = texts[i]
			}
			n := strconv.Itoa(i + 1)
			fx.Fields = append(fx.Fields, Field{Key: strings.ToLower(k.Field) + "_" + n,
				Label: strings.ToUpper(k.Name[:1]) + k.Name[1:] + " " + n, Value: v, Max: k.Limit})
		}
	}
	if len(fx.Fields) == 0 {
		return nil
	}
	fx.run = func(ctx context.Context, v map[string]string) error {
		var add []google.GroupAsset
		seen := map[string]bool{}
		for _, k := range materialKinds {
			for i := 1; k.Limit > 0 && i <= textRows; i++ {
				t := v[strings.ToLower(k.Field)+"_"+strconv.Itoa(i)]
				if t == "" {
					continue
				}
				if seen[k.Field+t] || g.Texts[strings.ToLower(t)] {
					return badInput("\"%s\" is already there", t)
				}
				seen[k.Field+t] = true
				add = append(add, google.GroupAsset{FieldType: k.Field, Text: t})
			}
		}
		if s := v["video_1"]; s != "" {
			id := youtubeIDOf(s)
			if id == "" {
				return badInput("the video is a YouTube link, for example https://youtu.be/abcdefghijk")
			}
			add = append(add, google.GroupAsset{FieldType: "YOUTUBE_VIDEO", YouTubeID: id})
		}
		if len(add) == 0 {
			return badInput("fill in at least one row")
		}
		return in.Ads.AddGroupAssets(ctx, in.CustomerID, g.ID, add)
	}
	return fx
}

// materialTexts proposes the business's own words that fit, leaving out texts the group has.
func materialTexts(k materialKind, p Profile, have map[string]bool) []string {
	src := p.Services
	if k.Field != "HEADLINE" {
		src = p.Descriptions
	}
	if k.Field == "HEADLINE" {
		src = append([]string{p.Name}, src...)
	}
	var out []string
	for _, s := range src {
		s = strings.TrimSpace(s)
		if s != "" && utf8.RuneCountInString(s) <= k.Limit && !have[strings.ToLower(s)] {
			out = append(out, s)
		}
	}
	return out
}
