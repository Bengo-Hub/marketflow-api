package worker

import "testing"

func TestMetaCAPIRowCTWA(t *testing.T) {
	web := metaCAPIRow(&ConversionEvent{EventName: "Purchase", Value: 1500, Currency: "KES"}, 1, map[string]any{"em": []string{"h"}}, "")
	if web["action_source"] != "website" {
		t.Fatalf("website: %v", web)
	}
	ctwa := metaCAPIRow(&ConversionEvent{EventName: "Purchase", CtwaClid: "ARAk"}, 1, map[string]any{"em": []string{"h"}}, "waba9")
	ud := ctwa["user_data"].(map[string]any)
	if ctwa["action_source"] != "business_messaging" || ctwa["messaging_channel"] != "whatsapp" || ud["ctwa_clid"] != "ARAk" || ud["whatsapp_business_account_id"] != "waba9" {
		t.Fatalf("ctwa: %v", ctwa)
	}
	if _, leaked := ud["em"]; leaked {
		t.Fatal("a messaging event carries the click id, not the hashed email")
	}
}

func TestCTWALeadIsAdAttributed(t *testing.T) {
	if !adLeadSources["ctwa"] {
		t.Fatal("a ctwa lead's sale must reach CAPI")
	}
}
