// Command meta-smoke makes read-only Graph API calls with a tenant's connected Meta accounts, one
// per permission, so Meta's App Review "Testing" page records a successful call for each. It
// prints only the permission and the result, never a token.
//
//	marketflow-meta-smoke -tenant codevertex [-ad-account act_123] [-ads-repeat 0] [-pause 2s]
//
// -ads-repeat adds that many extra ads_read insight reads (the Marketing API Access Tier needs
// 500 calls at an 85% success rate), spaced by -pause to stay well inside Meta's rate limits.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

func main() {
	slug := flag.String("tenant", "codevertex", "tenant slug whose Meta connection is used")
	adAccount := flag.String("ad-account", "", "ad account id (act_...); default: the first connected one")
	adsRepeat := flag.Int("ads-repeat", 0, "extra ads_read calls for the Marketing API Access Tier")
	writes := flag.Bool("writes", false, "also run the approved write tests (a Page post and comment, a paused campaign), all deleted after")
	writeAdAccount := flag.String("write-ad-account", "", "ad account for the paused test campaign (default: -ad-account)")
	pause := flag.Duration("pause", 2*time.Second, "pause between calls")
	flag.Parse()

	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("POSTGRES_URL"))
	if err != nil {
		log.Fatalf("open postgres: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	keys := encryption.NewKeyProvider(client, os.Getenv("MF_SECURITY_ENCRYPTION_KEY"))

	t, err := client.Tenant.Query().Where(tenant.SlugEQ(*slug)).Only(ctx)
	if err != nil {
		log.Fatalf("tenant %q: %v", *slug, err)
	}
	accounts, err := client.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(t.ID), connectedaccount.StatusEQ("active"), connectedaccount.PlatformEQ("meta")).
		All(ctx)
	if err != nil {
		log.Fatalf("connected accounts: %v", err)
	}
	var pageID, igID, adID, pageTok, userTok string
	for _, a := range accounts {
		tok, derr := keys.Decrypt(ctx, a.TokenCiphertext)
		if derr != nil {
			log.Printf("skip %s %s: token not readable", a.AccountType, a.ExternalID)
			continue
		}
		switch a.AccountType {
		case "facebook_page":
			if pageID == "" || (t.MetaPageID != nil && *t.MetaPageID == a.ExternalID) {
				pageID, pageTok = a.ExternalID, tok
			}
		case "instagram_business":
			if igID == "" {
				igID = a.ExternalID
			}
		case "meta_ad_account":
			if userTok == "" {
				userTok = tok
			}
			if (*adAccount == "" && adID == "") || a.ExternalID == *adAccount {
				adID = a.ExternalID
			}
		}
	}
	if userTok == "" {
		log.Fatalf("tenant %q has no readable Meta user token (connect Meta first)", *slug)
	}
	fmt.Printf("tenant %s: page %s, instagram %s, ad account %s\n", *slug, orNone(pageID), orNone(igID), orNone(adID))

	httpc := &http.Client{Timeout: 30 * time.Second,
		Transport: meta.NewProofTransport(nil, meta.PlatformSecret(client, keys, os.Getenv("MF_META_APP_SECRET")))}
	run := func(c call) bool {
		tok := userTok
		if c.Token == "page" {
			tok = pageTok
		}
		q := c.Query
		if q == nil {
			q = map[string][]string{}
		}
		q.Set("access_token", tok)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, graphBase+c.Path+"?"+q.Encode(), nil)
		resp, err := httpc.Do(req)
		if err != nil {
			fmt.Printf("%-28s FAIL  %v\n", c.Permission, err)
			return false
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Printf("%-28s OK\n", c.Permission)
			return true
		}
		var e struct {
			Error struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		fmt.Printf("%-28s FAIL  HTTP %d code %d: %s\n", c.Permission, resp.StatusCode, e.Error.Code, strings.TrimSpace(e.Error.Message))
		return false
	}

	ok, total := 0, 0
	for _, c := range smokeCalls(pageID, igID, adID) {
		total++
		if run(c) {
			ok++
		}
		time.Sleep(*pause)
	}
	if adID != "" && *adsRepeat > 0 {
		insights := smokeCalls("", "", adID)[3] // ads_read
		good := 0
		for i := 0; i < *adsRepeat; i++ {
			if run(insights) {
				good++
			}
			time.Sleep(*pause)
		}
		fmt.Printf("ads_read repeats: %d of %d succeeded\n", good, *adsRepeat)
	}
	if *writes {
		wAd := *writeAdAccount
		if wAd == "" {
			wAd = adID
		}
		runWrites(ctx, httpc, pageID, pageTok, wAd, userTok)
	}
	fmt.Printf("done: %d of %d permission calls succeeded\n", ok, total)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// graphBase is the Graph API root at MarketFlow's pinned version.
var graphBase = meta.GraphURL
