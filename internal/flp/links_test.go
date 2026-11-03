package flp

import "testing"

func TestForTenant(t *testing.T) {
	cases := []struct {
		name                                        string
		slug, distrib, store, lang, country         string
		ok                                          bool
		wantDistrib, wantStore, wantLang, wantShop string
	}{
		{name: "stored values win", slug: "jane-fbo", distrib: "254200047984", store: "uga", lang: "EN", country: "KE", ok: true,
			wantDistrib: "254200047984", wantStore: "UGA", wantLang: "en",
			wantShop: "https://shop.foreverliving.com/retail/entry/Shop.do?distribID=254200047984&language=en&store=UGA"},
		{name: "slug is the distributor ID, store from country", slug: "256700111222", country: "ug", ok: true,
			wantDistrib: "256700111222", wantStore: "UGA", wantLang: "en",
			wantShop: "https://shop.foreverliving.com/retail/entry/Shop.do?distribID=256700111222&language=en&store=UGA"},
		{name: "unknown country falls back to KEN", slug: "254711000000", country: "ZZ", ok: true,
			wantDistrib: "254711000000", wantStore: "KEN", wantLang: "en",
			wantShop: "https://shop.foreverliving.com/retail/entry/Shop.do?distribID=254711000000&language=en&store=KEN"},
		{name: "no distributor ID, no links", slug: "jane-fbo", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := ForTenant(tc.slug, tc.distrib, tc.store, tc.lang, tc.country)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if b.DistribID != tc.wantDistrib || b.StoreCode != tc.wantStore || b.Language != tc.wantLang {
				t.Fatalf("got %+v", b)
			}
			if got := b.ShopURL(); got != tc.wantShop {
				t.Fatalf("shop url\n got %s\nwant %s", got, tc.wantShop)
			}
		})
	}
}
