package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// graphWrite sends one POST or DELETE and returns the created object's id ("" for a delete).
func graphWrite(ctx context.Context, httpc *http.Client, method, path, token string, form url.Values) (string, error) {
	if form == nil {
		form = url.Values{}
	}
	form.Set("access_token", token)
	var req *http.Request
	if method == http.MethodDelete {
		req, _ = http.NewRequestWithContext(ctx, method, graphBase+path+"?"+form.Encode(), nil)
	} else {
		req, _ = http.NewRequestWithContext(ctx, method, graphBase+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var out struct {
		ID    string `json:"id"`
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d code %d: %s", resp.StatusCode, out.Error.Code, out.Error.Message)
	}
	return out.ID, nil
}

// runWrites tests the write permissions the owner approved on 2026-11-10, cleaning up after
// itself: a Page post with a Page comment (pages_manage_posts, pages_manage_engagement), both
// deleted; and a PAUSED campaign with no ad sets or ads (ads_management), deleted. A failed step
// still runs the clean-up of what was created.
func runWrites(ctx context.Context, httpc *http.Client, pageID, pageTok, adID, userTok string) {
	report := func(perm, step string, err error) {
		if err != nil {
			fmt.Printf("%-28s FAIL  %s: %v\n", perm, step, err)
			return
		}
		fmt.Printf("%-28s OK    %s\n", perm, step)
	}
	if pageID != "" && pageTok != "" {
		postID, err := graphWrite(ctx, httpc, http.MethodPost, "/"+pageID+"/feed", pageTok,
			url.Values{"message": {"MarketFlow test post, please ignore. It is removed in a moment."}})
		report("pages_manage_posts", "publish test post", err)
		if err == nil {
			commentID, cerr := graphWrite(ctx, httpc, http.MethodPost, "/"+postID+"/comments", pageTok,
				url.Values{"message": {"Test reply from MarketFlow."}})
			report("pages_manage_engagement", "comment on the post", cerr)
			if cerr == nil {
				_, derr := graphWrite(ctx, httpc, http.MethodDelete, "/"+commentID, pageTok, nil)
				report("pages_manage_engagement", "delete the comment", derr)
			}
			_, derr := graphWrite(ctx, httpc, http.MethodDelete, "/"+postID, pageTok, nil)
			report("pages_manage_posts", "delete the test post", derr)
		}
	}
	if adID != "" && userTok != "" {
		campID, err := graphWrite(ctx, httpc, http.MethodPost, "/"+adID+"/campaigns", userTok, url.Values{
			"name":                            {"MarketFlow API test (paused, no ads)"},
			"objective":                       {"OUTCOME_LEADS"},
			"status":                          {"PAUSED"},
			"special_ad_categories":           {"[]"},
			"is_adset_budget_sharing_enabled": {"false"},
		})
		report("ads_management", "create paused campaign", err)
		if err == nil {
			_, derr := graphWrite(ctx, httpc, http.MethodDelete, "/"+campID, userTok, nil)
			report("ads_management", "delete the campaign", derr)
		}
	}
}
