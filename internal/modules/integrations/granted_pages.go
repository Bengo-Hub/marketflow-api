package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"go.uber.org/zap"
)

// pageGrantScopes are the permissions whose granular target ids are Facebook Pages.
var pageGrantScopes = map[string]bool{
	"pages_show_list": true, "pages_manage_posts": true, "pages_read_engagement": true,
	"pages_manage_metadata": true, "pages_messaging": true, "pages_manage_engagement": true,
	"pages_read_user_content": true, "pages_manage_ads": true,
}

// grantedPages lists the Pages a person granted through Facebook Login for Business when
// /me/accounts comes back empty (Pages held through a business portfolio). debug_token's
// granular_scopes names each granted Page; each is then read with the user token for its Page
// token. Unreadable Pages are skipped. appToken is "app_id|app_secret".
func grantedPages(ctx context.Context, userToken, appToken string) ([]metaPage, error) {
	var dbg struct {
		Data struct {
			GranularScopes []struct {
				Scope     string   `json:"scope"`
				TargetIDs []string `json:"target_ids"`
			} `json:"granular_scopes"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := graphGet(ctx, "/debug_token", url.Values{"input_token": {userToken}, "access_token": {appToken}}, &dbg); err != nil {
		return nil, err
	}
	if dbg.Error != nil {
		return nil, fmt.Errorf("debug_token: %s", dbg.Error.Message)
	}
	var ids []string
	seen := map[string]bool{}
	for _, g := range dbg.Data.GranularScopes {
		if !pageGrantScopes[g.Scope] {
			continue
		}
		for _, id := range g.TargetIDs {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	var pages []metaPage
	for _, id := range ids {
		var p struct {
			metaPage
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := graphGet(ctx, "/"+url.PathEscape(id), url.Values{
			"access_token": {userToken},
			"fields":       {"id,name,access_token,instagram_business_account{id,username,profile_picture_url}"},
		}, &p); err != nil || p.Error != nil || p.ID == "" || p.AccessToken == "" {
			continue
		}
		pages = append(pages, p.metaPage)
	}
	return pages, nil
}

// graphGet reads one Graph object through graphClient (which signs appsecret_proof).
func graphGet(ctx context.Context, path string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaGraphURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := graphClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("graph %s: %w", path, err)
	}
	return nil
}

var (
	errStateInvalid = errors.New("connect link is not valid")
	errStateExpired = errors.New("connect link expired; start the connection again")
)

// stateReason is the reason code a callback reports for a bad state.
func stateReason(err error) string {
	if errors.Is(err, errStateExpired) {
		return "state_expired"
	}
	return "bad_state"
}

// failConnect ends a connect callback that did not work. The browser is on the API host, so it
// goes back to the tenant's integrations page with `<provider>=error&reason=<code>` for the page
// to explain; the detail is logged, never put in the URL. Without a known tenant (a forged
// state) it goes to the portal home.
func (h *Handler) failConnect(w http.ResponseWriter, r *http.Request, provider, tenantID, reason, detail string) {
	if h.logger != nil {
		h.logger.Warn("connect failed", zap.String("provider", provider), zap.String("tenant", tenantID),
			zap.String("reason", reason), zap.String("detail", detail))
	}
	target := h.portalURL + "/"
	if tenantID != "" {
		target = h.integrationsURL(r.Context(), tenantID, provider+"=error&reason="+url.QueryEscape(reason))
	}
	http.Redirect(w, r, target, http.StatusFound)
}
