package google

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Google Ads API calls of the ads setup assistant (plan item 65). Reads are one GAQL query each,
// so one refused query leaves the other checks working. Writes run only from the assistant's Fix
// button, after the owner has seen the change.

// SetupAccount is the account-level part of the checklist.
type SetupAccount struct {
	Status      string // ENABLED, CANCELED, SUSPENDED, CLOSED
	AutoTagging bool
	Currency    string
}

// SetupAccount reads the account's status, currency and auto-tagging.
func (a Ads) SetupAccount(ctx context.Context, customerID string) (SetupAccount, error) {
	rows, err := a.search(ctx, customerID,
		"SELECT customer.status, customer.auto_tagging_enabled, customer.currency_code FROM customer LIMIT 1")
	if err != nil {
		return SetupAccount{}, err
	}
	if len(rows) == 0 {
		return SetupAccount{}, fmt.Errorf("google ads returned no account")
	}
	c := obj(rows[0], "customer")
	// A proto3 false is left out of the JSON, so a missing autoTaggingEnabled is off.
	tag, _ := c["autoTaggingEnabled"].(bool)
	return SetupAccount{Status: text(c, "status"), AutoTagging: tag, Currency: text(c, "currencyCode")}, nil
}

// ManagerLink is one manager account linked, or invited, to the customer.
type ManagerLink struct {
	ManagerID string // digits
	Status    string // ACTIVE, PENDING, INACTIVE, REFUSED, CANCELED
}

// ManagerLinks lists the customer's manager links.
func (a Ads) ManagerLinks(ctx context.Context, customerID string) ([]ManagerLink, error) {
	rows, err := a.search(ctx, customerID,
		"SELECT customer_manager_link.manager_customer, customer_manager_link.status FROM customer_manager_link")
	if err != nil {
		return nil, err
	}
	out := make([]ManagerLink, 0, len(rows))
	for _, r := range rows {
		l := obj(r, "customerManagerLink")
		out = append(out, ManagerLink{ManagerID: strings.TrimPrefix(text(l, "managerCustomer"), "customers/"), Status: text(l, "status")})
	}
	return out, nil
}

// SetupAction is an enabled conversion action with the goal (category and origin) it counts for.
type SetupAction struct {
	ResourceName, Name, Type, Category, Origin string
	Primary                                    bool
}

// SetupActions lists the account's enabled conversion actions of every type.
func (a Ads) SetupActions(ctx context.Context, customerID string) ([]SetupAction, error) {
	rows, err := a.search(ctx, customerID, "SELECT conversion_action.resource_name, conversion_action.name, conversion_action.type, "+
		"conversion_action.category, conversion_action.origin, conversion_action.primary_for_goal FROM conversion_action "+
		"WHERE conversion_action.status = 'ENABLED' LIMIT 500")
	if err != nil {
		return nil, err
	}
	out := make([]SetupAction, 0, len(rows))
	for _, r := range rows {
		c := obj(r, "conversionAction")
		p, _ := c["primaryForGoal"].(bool)
		out = append(out, SetupAction{ResourceName: text(c, "resourceName"), Name: text(c, "name"), Type: text(c, "type"),
			Category: text(c, "category"), Origin: text(c, "origin"), Primary: p})
	}
	return out, nil
}

// NewAction is an offline (upload click) conversion action to create.
type NewAction struct {
	Name, Category, Currency string
	Value                    float64
	WindowDays               int
}

// CreateOfflineAction creates the conversion action, counting one per click, and returns its
// resource name.
func (a Ads) CreateOfflineAction(ctx context.Context, customerID string, n NewAction) (string, error) {
	names, err := a.mutate(ctx, customerID, "conversionActions", []map[string]any{{"create": map[string]any{
		"name": n.Name, "type": "UPLOAD_CLICKS", "category": n.Category, "status": "ENABLED",
		"countingType": "ONE_PER_CLICK", "clickThroughLookbackWindowDays": strconv.Itoa(n.WindowDays), "primaryForGoal": true,
		"valueSettings": map[string]any{"defaultValue": n.Value, "defaultCurrencyCode": n.Currency, "alwaysUseDefaultValue": false},
	}}})
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("google ads created no conversion action")
	}
	return names[0], nil
}

// SetupCampaign is an enabled campaign and how it is doing.
type SetupCampaign struct {
	ID, ResourceName, Name, PrimaryStatus string
	Reasons                               []string
}

// SetupCampaigns lists the enabled campaigns.
func (a Ads) SetupCampaigns(ctx context.Context, customerID string) ([]SetupCampaign, error) {
	rows, err := a.search(ctx, customerID, "SELECT campaign.resource_name, campaign.id, campaign.name, "+
		"campaign.primary_status, campaign.primary_status_reasons FROM campaign WHERE campaign.status = 'ENABLED' LIMIT 200")
	if err != nil {
		return nil, err
	}
	out := make([]SetupCampaign, 0, len(rows))
	for _, r := range rows {
		c := obj(r, "campaign")
		sc := SetupCampaign{ID: idText(c["id"]), ResourceName: text(c, "resourceName"), Name: text(c, "name"),
			PrimaryStatus: text(c, "primaryStatus")}
		rs, _ := c["primaryStatusReasons"].([]any)
		for _, x := range rs {
			if s, ok := x.(string); ok {
				sc.Reasons = append(sc.Reasons, s)
			}
		}
		out = append(out, sc)
	}
	return out, nil
}

// SetupGoal is one conversion goal: Biddable means campaigns optimise for it. Campaign is empty
// for the account's own default goals.
type SetupGoal struct {
	Campaign, Category, Origin string
	Biddable                   bool
}

// SetupGoals reads the account's default goals, each enabled campaign's goals, and which campaigns
// keep their own goal list (the others follow the account's defaults).
func (a Ads) SetupGoals(ctx context.Context, customerID string) (account, campaigns []SetupGoal, ownGoals map[string]bool, err error) {
	rows, err := a.search(ctx, customerID, "SELECT customer_conversion_goal.category, "+
		"customer_conversion_goal.origin, customer_conversion_goal.biddable FROM customer_conversion_goal")
	if err != nil {
		return nil, nil, nil, err
	}
	account = goalRows(rows, "customerConversionGoal")
	rows, err = a.search(ctx, customerID, "SELECT campaign_conversion_goal.campaign, "+
		"campaign_conversion_goal.category, campaign_conversion_goal.origin, campaign_conversion_goal.biddable "+
		"FROM campaign_conversion_goal WHERE campaign.status = 'ENABLED'")
	if err != nil {
		return nil, nil, nil, err
	}
	campaigns = goalRows(rows, "campaignConversionGoal")
	rows, err = a.search(ctx, customerID, "SELECT conversion_goal_campaign_config.campaign, conversion_goal_campaign_config.goal_config_level "+
		"FROM conversion_goal_campaign_config WHERE campaign.status = 'ENABLED'")
	if err != nil {
		return nil, nil, nil, err
	}
	ownGoals = map[string]bool{}
	for _, r := range rows {
		c := obj(r, "conversionGoalCampaignConfig")
		if text(c, "goalConfigLevel") == "CAMPAIGN" {
			ownGoals[text(c, "campaign")] = true
		}
	}
	return account, campaigns, ownGoals, nil
}

func goalRows(rows []map[string]any, key string) []SetupGoal {
	out := make([]SetupGoal, 0, len(rows))
	for _, r := range rows {
		g := obj(r, key)
		b, _ := g["biddable"].(bool)
		out = append(out, SetupGoal{Campaign: text(g, "campaign"), Category: text(g, "category"), Origin: text(g, "origin"), Biddable: b})
	}
	return out
}

// AddAccountGoal makes the goal one of the account's defaults.
func (a Ads) AddAccountGoal(ctx context.Context, customerID, category, origin string) error {
	id := CustomerDigits(customerID)
	_, err := a.mutate(ctx, id, "customerConversionGoals", []map[string]any{{"updateMask": "biddable", "update": map[string]any{
		"resourceName": fmt.Sprintf("customers/%s/customerConversionGoals/%s~%s", id, category, origin), "biddable": true}}})
	return err
}

// AddCampaignGoals adds the goal to campaigns that keep their own goal list.
func (a Ads) AddCampaignGoals(ctx context.Context, customerID string, campaignIDs []string, category, origin string) error {
	id := CustomerDigits(customerID)
	ops := make([]map[string]any, 0, len(campaignIDs))
	for _, c := range campaignIDs {
		ops = append(ops, map[string]any{"updateMask": "biddable", "update": map[string]any{
			"resourceName": fmt.Sprintf("customers/%s/campaignConversionGoals/%s~%s~%s", id, c, category, origin), "biddable": true}})
	}
	_, err := a.mutate(ctx, id, "campaignConversionGoals", ops)
	return err
}

// AssetCount holds enabled asset links per field type (SITELINK, CALL, CALLOUT).
type AssetCount struct {
	Account   map[string]int
	Campaigns map[string]map[string]int // by campaign id
}

// AssetCounts counts the sitelinks, calls and callouts on the account and on each campaign.
func (a Ads) AssetCounts(ctx context.Context, customerID string) (AssetCount, error) {
	out := AssetCount{Account: map[string]int{}, Campaigns: map[string]map[string]int{}}
	rows, err := a.search(ctx, customerID, "SELECT customer_asset.field_type FROM customer_asset "+
		"WHERE customer_asset.status = 'ENABLED' AND customer_asset.field_type IN ('SITELINK','CALL','CALLOUT')")
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		out.Account[text(obj(r, "customerAsset"), "fieldType")]++
	}
	rows, err = a.search(ctx, customerID, "SELECT campaign.id, campaign_asset.field_type FROM campaign_asset "+
		"WHERE campaign.status = 'ENABLED' AND campaign_asset.status = 'ENABLED' AND campaign_asset.field_type IN ('SITELINK','CALL','CALLOUT')")
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		id := idText(obj(r, "campaign")["id"])
		if out.Campaigns[id] == nil {
			out.Campaigns[id] = map[string]int{}
		}
		out.Campaigns[id][text(obj(r, "campaignAsset"), "fieldType")]++
	}
	return out, nil
}

// NewAsset is one account-level asset. FieldType decides which fields are used.
type NewAsset struct {
	FieldType      string // SITELINK, CALL or CALLOUT
	Text           string // sitelink text or callout
	URL            string // sitelink
	Country, Phone string // call
}

// CreateAccountAssets creates the assets and links them to the whole account.
func (a Ads) CreateAccountAssets(ctx context.Context, customerID string, assets []NewAsset) error {
	ops := make([]map[string]any, 0, len(assets))
	for _, x := range assets {
		var c map[string]any
		switch x.FieldType {
		case "SITELINK":
			c = map[string]any{"finalUrls": []string{x.URL}, "sitelinkAsset": map[string]any{"linkText": x.Text}}
		case "CALL":
			c = map[string]any{"callAsset": map[string]any{"countryCode": x.Country, "phoneNumber": x.Phone}}
		case "CALLOUT":
			c = map[string]any{"calloutAsset": map[string]any{"calloutText": x.Text}}
		default:
			return fmt.Errorf("unknown asset type %q", x.FieldType)
		}
		ops = append(ops, map[string]any{"create": c})
	}
	names, err := a.mutate(ctx, customerID, "assets", ops)
	if err != nil {
		return err
	}
	if len(names) != len(assets) {
		return fmt.Errorf("google ads created %d of %d assets", len(names), len(assets))
	}
	links := make([]map[string]any, 0, len(names))
	for i, n := range names {
		links = append(links, map[string]any{"create": map[string]any{"asset": n, "fieldType": assets[i].FieldType}})
	}
	_, err = a.mutate(ctx, customerID, "customerAssets", links)
	return err
}

// UserList is a customer list (Customer Match); sizes are 0 until Google knows them.
type UserList struct {
	Name                    string
	SearchSize, DisplaySize int64
}

// CustomerLists lists the account's open Customer Match lists.
func (a Ads) CustomerLists(ctx context.Context, customerID string) ([]UserList, error) {
	rows, err := a.search(ctx, customerID, "SELECT user_list.name, user_list.size_for_search, user_list.size_for_display "+
		"FROM user_list WHERE user_list.type = 'CRM_BASED' AND user_list.membership_status = 'OPEN' LIMIT 100")
	if err != nil {
		return nil, err
	}
	out := make([]UserList, 0, len(rows))
	for _, r := range rows {
		u := obj(r, "userList")
		out = append(out, UserList{Name: text(u, "name"), SearchSize: int64Of(u["sizeForSearch"]), DisplaySize: int64Of(u["sizeForDisplay"])})
	}
	return out, nil
}

// LimitedAd is an ad Google disapproved or limits.
type LimitedAd struct {
	Campaign, Approval string
	Topics             []string
}

// LimitedAds lists enabled ads with a policy problem.
func (a Ads) LimitedAds(ctx context.Context, customerID string) ([]LimitedAd, error) {
	rows, err := a.search(ctx, customerID, "SELECT campaign.name, ad_group_ad.policy_summary.approval_status, "+
		"ad_group_ad.policy_summary.policy_topic_entries FROM ad_group_ad WHERE ad_group_ad.status = 'ENABLED' AND campaign.status = 'ENABLED' "+
		"AND ad_group_ad.policy_summary.approval_status IN ('DISAPPROVED','APPROVED_LIMITED','AREA_OF_INTEREST_ONLY') LIMIT 50")
	if err != nil {
		return nil, err
	}
	out := make([]LimitedAd, 0, len(rows))
	for _, r := range rows {
		ps := obj(obj(r, "adGroupAd"), "policySummary")
		ad := LimitedAd{Campaign: text(obj(r, "campaign"), "name"), Approval: text(ps, "approvalStatus")}
		ts, _ := ps["policyTopicEntries"].([]any)
		for _, t := range ts {
			if m, ok := t.(map[string]any); ok && text(m, "topic") != "" {
				ad.Topics = append(ad.Topics, text(m, "topic"))
			}
		}
		out = append(out, ad)
	}
	return out, nil
}

// EnableAutoTagging switches auto-tagging on.
func (a Ads) EnableAutoTagging(ctx context.Context, customerID string) error {
	id := CustomerDigits(customerID)
	return a.do(ctx, http.MethodPost, "/customers/"+id+":mutate", map[string]any{"operation": map[string]any{
		"updateMask": "autoTaggingEnabled", "update": map[string]any{"resourceName": "customers/" + id, "autoTaggingEnabled": true}}}, nil)
}

// mutate runs operations on one collection and returns the resource names. Google applies all of
// them or none (no partial failure).
func (a Ads) mutate(ctx context.Context, customerID, collection string, ops []map[string]any) ([]string, error) {
	var out struct {
		Results []struct {
			ResourceName string `json:"resourceName"`
		} `json:"results"`
	}
	if err := a.do(ctx, http.MethodPost, "/customers/"+CustomerDigits(customerID)+"/"+collection+":mutate",
		map[string]any{"operations": ops}, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Results))
	for _, r := range out.Results {
		names = append(names, r.ResourceName)
	}
	return names, nil
}

func obj(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func text(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// idText is an int64 id, which Google sends as a JSON string.
func idText(v any) string {
	if n := int64Of(v); n > 0 {
		return strconv.FormatInt(n, 10)
	}
	return ""
}
