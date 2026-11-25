package google

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Performance Max ad material and manager links, for the ads setup assistant (plan item 68).

// AssetGroup is an enabled asset group of an enabled campaign with what it holds.
type AssetGroup struct {
	ID, Name, Strength string
	Count              map[string]int  // by field type
	Texts              map[string]bool // lower case
	Videos             map[string]bool // YouTube ids
}

// AssetGroups lists the asset groups of running Performance Max campaigns.
func (a Ads) AssetGroups(ctx context.Context, customerID string) ([]AssetGroup, error) {
	rows, err := a.search(ctx, customerID, "SELECT asset_group.id, asset_group.name, asset_group.ad_strength, asset_group.status, campaign.status "+
		"FROM asset_group WHERE asset_group.status = 'ENABLED' AND campaign.status = 'ENABLED' LIMIT 20")
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var out []AssetGroup
	at := map[string]int{}
	for _, r := range rows {
		g := obj(r, "assetGroup")
		id := idText(g["id"])
		at[id] = len(out)
		out = append(out, AssetGroup{ID: id, Name: text(g, "name"), Strength: text(g, "adStrength"),
			Count: map[string]int{}, Texts: map[string]bool{}, Videos: map[string]bool{}})
	}
	rows, err = a.search(ctx, customerID, "SELECT asset_group.id, asset_group.status, campaign.status, asset_group_asset.field_type, asset_group_asset.status, "+
		"asset.text_asset.text, asset.youtube_video_asset.youtube_video_id FROM asset_group_asset "+
		"WHERE asset_group.status = 'ENABLED' AND campaign.status = 'ENABLED' AND asset_group_asset.status = 'ENABLED' LIMIT 2000")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		i, ok := at[idText(obj(r, "assetGroup")["id"])]
		if !ok {
			continue
		}
		g, as := out[i], obj(r, "asset")
		g.Count[text(obj(r, "assetGroupAsset"), "fieldType")]++
		if t := text(obj(as, "textAsset"), "text"); t != "" {
			g.Texts[strings.ToLower(t)] = true
		}
		if v := text(obj(as, "youtubeVideoAsset"), "youtubeVideoId"); v != "" {
			g.Videos[v] = true
		}
	}
	return out, nil
}

// GroupAsset is a text (headline, long headline, description) or a YouTube video to add.
type GroupAsset struct{ FieldType, Text, YouTubeID string }

// AddGroupAssets creates the assets and puts them in the asset group, all or nothing.
func (a Ads) AddGroupAssets(ctx context.Context, customerID, groupID string, add []GroupAsset) error {
	cid := CustomerDigits(customerID)
	var assets, links []map[string]any
	for i, x := range add {
		rn := "customers/" + cid + "/assets/" + strconv.Itoa(-1-i)
		c := map[string]any{"resourceName": rn}
		if x.YouTubeID != "" {
			c["youtubeVideoAsset"] = map[string]any{"youtubeVideoId": x.YouTubeID}
		} else {
			c["textAsset"] = map[string]any{"text": x.Text}
		}
		assets = append(assets, map[string]any{"assetOperation": map[string]any{"create": c}})
		links = append(links, map[string]any{"assetGroupAssetOperation": map[string]any{"create": map[string]any{
			"assetGroup": "customers/" + cid + "/assetGroups/" + groupID, "asset": rn, "fieldType": x.FieldType}}})
	}
	return a.do(ctx, http.MethodPost, "/customers/"+cid+"/googleAds:mutate", map[string]any{"mutateOperations": append(assets, links...)}, nil)
}

// RequestManagerLink invites the customer from the manager account. The signed-in person needs
// access to the manager.
func (a Ads) RequestManagerLink(ctx context.Context, managerID, customerID string) error {
	m := CustomerDigits(managerID)
	a.LoginCustomerID = m
	return a.do(ctx, http.MethodPost, "/customers/"+m+"/customerClientLinks:mutate", map[string]any{"operation": map[string]any{
		"create": map[string]any{"clientCustomer": "customers/" + CustomerDigits(customerID), "status": "PENDING"}}}, nil)
}

// AcceptManagerLink accepts a waiting invitation, as the customer.
func (a Ads) AcceptManagerLink(ctx context.Context, customerID string, l ManagerLink) error {
	if l.ResourceName == "" {
		return fmt.Errorf("google ads gave no link to accept")
	}
	_, err := a.mutate(ctx, customerID, "customerManagerLinks", []map[string]any{
		{"updateMask": "status", "update": map[string]any{"resourceName": l.ResourceName, "status": "ACTIVE"}}})
	return err
}
