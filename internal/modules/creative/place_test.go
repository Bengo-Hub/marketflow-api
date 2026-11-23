package creative

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

func TestContactsForFollowTheConfiguredOrder(t *testing.T) {
	wa := "+254 712 345 678"
	tn := &ent.Tenant{WhatsappNumber: &wa, Metadata: map[string]any{"contact_email": "hello@example.com"}}
	facts := marketingos.BusinessFacts{Phone: "0712345678"}
	got := contactsFor("whatsapp,phone,link,email", tn, facts, "https://digitika.example/demo", "https://codevertex.example")
	if len(got) != 3 || got[0].Kind != "whatsapp" || got[1].Kind != "link" || got[1].Value != "https://digitika.example/demo" || got[2].Kind != "email" {
		t.Fatalf("contacts %+v: the phone equal to WhatsApp is dropped, the link is the offering page", got)
	}
	got = contactsFor("web,fax", tn, facts, "", "")
	if len(got) != 0 {
		t.Fatalf("nothing to show, got %+v", got)
	}
	if got := contactsFor("link", tn, facts, "not a url", "https://codevertex.example"); len(got) != 1 || got[0].Value != "https://codevertex.example" {
		t.Fatalf("a link falls back to the website: %+v", got)
	}
}

func TestPlaceLine(t *testing.T) {
	p := placeInfo{Facts: placeFacts{Address: "Pioneer House, Oginga Street, Kisumu CBD", City: "Kisumu", Country: "Kenya"}}
	if got := p.Line(); got != "Pioneer House, Oginga Street, Kisumu CBD, Kenya" {
		t.Fatalf("line %q", got)
	}
}

func TestDownloadName(t *testing.T) {
	a := &ent.CreativeAsset{ID: uuid.MustParse("12345678-1111-2222-3333-444444444444"), Kind: "video"}
	if got := downloadName(a, "/t/v/ad.mp4", "video/mp4"); got != "marketflow-video-12345678.mp4" {
		t.Fatalf("name %q", got)
	}
	if got := downloadName(a, "/t/v/poster", "image/png"); !strings.HasSuffix(got, ".png") {
		t.Fatalf("name %q", got)
	}
}

func TestReadablePhone(t *testing.T) {
	for in, want := range map[string]string{"254743793901": "+254 743 793 901", "0712345678": "+254 712 345 678", "+254 743 793 901": "+254 743 793 901", "+1 415 555 0100": "+1 415 555 0100"} {
		if got := readablePhone(in); got != want {
			t.Fatalf("%q = %q, want %q", in, got, want)
		}
	}
}
