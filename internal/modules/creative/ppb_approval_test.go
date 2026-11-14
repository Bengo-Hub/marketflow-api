package creative

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
)

// A pharmacy's medicine ad needs a PPB advert approval reference (owner, 2026-11-14); products that
// are not medicines and every other business type are never held.
func TestIsMedicineOffering(t *testing.T) {
	cases := []struct {
		offering map[string]any
		title    string
		want     bool
	}{
		{map[string]any{"name": "Panadol Extra 500mg"}, "", true},
		{map[string]any{"name": "Benylin cough syrup 100ml"}, "", true},
		{map[string]any{"name": "Ozempic 1mg pen"}, "", true},
		{map[string]any{"name": "Coartem 20/120", "category": "Antimalarials"}, "", true},
		{map[string]any{"name": "ORS sachets"}, "", true},
		{map[string]any{"name": "Optrex eye drops"}, "", true},
		{map[string]any{"name": "Metformin 500mg tablets"}, "", true},
		{map[string]any{"name": "Dawa ya kikohozi 100ml"}, "", true},
		{map[string]any{"name": "Dawa ya meno na dawa ya malaria"}, "", true},
		{map[string]any{"name": "Dawa ya meno na Panadol"}, "", true},
		{nil, "Punguzo kwa dawa ya meno na Panadol", true},
		{nil, "Punguzo kwa dawa ya meno na dawa ya mbu", false},
		{nil, "Mounjaro for weight management", true},
		// supplements are not held (owner, 2026-11-14)
		{map[string]any{"name": "Vitamin C 1000mg"}, "", false},
		{map[string]any{"name": "Seven Seas", "description": "Omega 3 supplement capsules"}, "", false},
		{map[string]any{"name": "Centrum multivitamin tablets"}, "", false},
		{map[string]any{"name": "Cod liver oil capsules"}, "", false},
		// household and cosmetic "dawa" is not medicine (owner, 2026-11-14)
		{map[string]any{"name": "Dawa ya meno Colgate 100ml"}, "", false},
		{map[string]any{"name": "Dawa ya mbu"}, "", false},
		{map[string]any{"name": "Dawa ya nywele"}, "", false},
		{map[string]any{"name": "Dawa ya kuosha vyombo"}, "", false},
		{map[string]any{"name": "Dawa za wadudu"}, "", false},
		// not medicines
		{map[string]any{"name": "Shea butter 250g"}, "", false},
		{map[string]any{"name": "Nivea body lotion 400ml"}, "", false},
		{map[string]any{"name": "Vitamin C serum 30ml"}, "", false},
		{map[string]any{"name": "Omron BP machine"}, "", false},
		{map[string]any{"name": "Accu-Chek glucometer"}, "", false},
		{map[string]any{"name": "Hand sanitiser 500ml"}, "", false},
		{map[string]any{"name": "Free blood pressure check", "type": "SERVICE", "description": "No tablets, just a quick check"}, "", false},
		{nil, "Weekend lotion offer", false},
		// an offering's name wins over the title
		{map[string]any{"name": "Baby wipes"}, "Panadol weekend", false},
	}
	for _, c := range cases {
		if got := IsMedicineOffering(c.offering, c.title); got != c.want {
			t.Errorf("IsMedicineOffering(%v, %q) = %v, want %v", c.offering, c.title, got, c.want)
		}
	}
}

// Only promotional adverts are held; availability and opening hours are not (owner, 2026-11-14).
func TestIsPromotionalText(t *testing.T) {
	promotional := []string{
		"Panadol Extra, KES 50 for 10 tablets",
		"Panadol at 150 bob",
		"20% off Panadol this week",
		"Order now on WhatsApp",
		"Buy Panadol today",
		"DM us to get yours",
		"Fast relief from headaches",
		"For pain and fever",
		"Coartem treats malaria",
		"Works in 15 minutes",
		"Nunua Panadol leo",
		"Panadol kwa maumivu ya kichwa",
	}
	availability := []string{
		"Panadol in stock",
		"Now stocking Coartem",
		"We are open till 9pm",
		"Open Monday to Saturday, 8am to 9pm",
		"Ask our pharmacist",
		"Prescription only",
		"Visit our Westlands branch",
		"PPB approval ref. PPB/SALE/1",
		"",
	}
	for _, s := range promotional {
		if !IsPromotionalText(s) {
			t.Errorf("%q must be promotional", s)
		}
	}
	for _, s := range availability {
		if IsPromotionalText(s) {
			t.Errorf("%q is availability only, not promotional", s)
		}
	}
	// A version promotes through any of its words, a price, or a video ad's script.
	if versionPromotes(&ent.CreativeVersion{Copy: map[string]any{"headline": "Panadol in stock", "subline": "Open till 9pm"}}) {
		t.Fatal("availability poster must not be held")
	}
	if !versionPromotes(&ent.CreativeVersion{Copy: map[string]any{"headline": "Panadol in stock", "price": "50"}}) {
		t.Fatal("a price makes it promotional")
	}
	script := map[string]any{"scenes": []any{map[string]any{"voiceover": "Panadol gives fast relief", "caption": ""}}}
	if !versionPromotes(&ent.CreativeVersion{Copy: map[string]any{"script": script, "end_card": map[string]any{"headline": "In stock"}}}) {
		t.Fatal("a video ad's script counts")
	}
}

func TestOfferingKey(t *testing.T) {
	cases := []struct {
		offering map[string]any
		title    string
		key      string
		name     string
	}{
		{map[string]any{"source": "inventory", "source_id": "42", "name": "Panadol Extra"}, "", "inventory:42", "Panadol Extra"},
		{map[string]any{"source_id": "42", "name": "Panadol Extra"}, "", "offering:42", "Panadol Extra"},
		{map[string]any{"name": "  Panadol   EXTRA "}, "", "name:panadol extra", "Panadol   EXTRA"},
		{nil, "Ozempic launch", "name:ozempic launch", "Ozempic launch"},
		{nil, "  ", "", ""},
	}
	for _, c := range cases {
		key, name := OfferingKey(c.offering, c.title)
		if key != c.key || name != c.name {
			t.Errorf("OfferingKey(%v, %q) = %q, %q; want %q, %q", c.offering, c.title, key, name, c.key, c.name)
		}
	}
	long := strings.Repeat("é", 150)
	if key, _ := OfferingKey(map[string]any{"name": long}, ""); len(key) > maxPPBOfferingKey || !strings.HasPrefix(key, "name:") {
		t.Fatalf("long key not cut cleanly: %d bytes", len(key))
	}
}

func TestNormalizePPBReference(t *testing.T) {
	for in, want := range map[string]string{
		"PPB/ADV/2025/0123":   "PPB/ADV/2025/0123",
		"  ppb adv   123  ":   "ppb adv 123",
		"PPB-ADV-#12 (radio)": "PPB-ADV-#12 (radio)",
	} {
		got, err := NormalizePPBReference(in)
		if err != nil || got != want {
			t.Errorf("NormalizePPBReference(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ab", "<script>", "/leading-slash", strings.Repeat("A", 81), "ref\x00x"} {
		if _, err := NormalizePPBReference(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizePPBReference(%q) must be invalid, got %v", bad, err)
		}
	}
}

func TestWithPPBLine(t *testing.T) {
	ref := "PPB/ADV/1"
	cases := map[string]string{
		"":                    "PPB approval ref. PPB/ADV/1",
		"For pain and fever":  "For pain and fever. PPB approval ref. PPB/ADV/1",
		"For pain and fever.": "For pain and fever. PPB approval ref. PPB/ADV/1",
		"Already carries PPB approval ref. PPB/ADV/1": "Already carries PPB approval ref. PPB/ADV/1",
	}
	for in, want := range cases {
		if got := withPPBLine(in, ref); got != want {
			t.Errorf("withPPBLine(%q) = %q, want %q", in, got, want)
		}
	}
	long := withPPBLine(strings.Repeat("word ", 60), ref)
	if len(long) > maxCopyLen || !strings.HasSuffix(long, "PPB approval ref. PPB/ADV/1") {
		t.Fatalf("long subline: %d characters, %q", len(long), long)
	}
}

func TestPPBStatus(t *testing.T) {
	medicine := &ent.CreativeBrief{Title: "Panadol week", Offering: map[string]any{"source": "inventory", "source_id": "7", "name": "Panadol 500mg"}}
	lotion := &ent.CreativeBrief{Title: "Lotion week", Offering: map[string]any{"name": "Body lotion"}}
	refs := map[string]any{PPBApprovalsMetadataKey: []any{
		map[string]any{"offering_key": "inventory:7", "reference": "PPB/ADV/7", "entered_at": "2026-11-14T08:00:00Z"},
		map[string]any{"offering_key": "inventory:8", "reference": " "}, // a bad entry is skipped
	}}

	pharmacy := &ent.Tenant{BusinessType: "pharmacy"}
	if st := ppbStatus(pharmacy, medicine); !st.Required || !st.Held() || st.OfferingKey != "inventory:7" || st.OfferingName != "Panadol 500mg" {
		t.Fatalf("pharmacy medicine ad without a reference must be held: %+v", st)
	}
	if st := ppbStatus(pharmacy, lotion); st.Required || st.Held() {
		t.Fatalf("a lotion is never held: %+v", st)
	}
	withRef := &ent.Tenant{BusinessType: "pharmacy", Metadata: refs}
	st := ppbStatus(withRef, medicine)
	if st.Held() || st.Approval == nil || st.Approval.Reference != "PPB/ADV/7" || st.CaptionLine != "PPB approval ref. PPB/ADV/7" {
		t.Fatalf("reference not found: %+v", st)
	}
	if got := ReadPPBApprovals(refs); len(got) != 1 {
		t.Fatalf("bad entries must be skipped: %+v", got)
	}
	// Other business types, the platform owner's included, are never held.
	for _, bt := range []string{"generic", "flp", "retail", "salon", ""} {
		if st := ppbStatus(&ent.Tenant{BusinessType: bt}, medicine); st.Required || st.Held() {
			t.Fatalf("business type %q must not be held: %+v", bt, st)
		}
	}
}

// The 409 the Studio turns into the reference card.
func TestPPBRequiredResponse(t *testing.T) {
	h := &Handler{logger: zap.NewNop()}
	w := httptest.NewRecorder()
	h.fail(w, &PPBApprovalRequiredError{OfferingKey: "inventory:7", OfferingName: "Panadol 500mg"})
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Error   string         `json:"error"`
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "ppb_approval_required" || body.Details["offering_key"] != "inventory:7" || !strings.Contains(body.Error, "Panadol 500mg") {
		t.Fatalf("unexpected body: %+v", body)
	}
	if strings.Contains(body.Error, "—") {
		t.Fatal("the message must not use an em dash")
	}
	if !errors.Is(&PPBApprovalRequiredError{}, ErrPPBApprovalRequired) {
		t.Fatal("errors.Is must match ErrPPBApprovalRequired")
	}
}

// The profile PATCH must not overwrite the references: the key is reserved in the tenant package.
func TestPPBApprovalsKeyIsReserved(t *testing.T) {
	if tenant.PPBApprovalsMetadataKey != PPBApprovalsMetadataKey {
		t.Fatalf("tenant and creative disagree on the key: %q vs %q", tenant.PPBApprovalsMetadataKey, PPBApprovalsMetadataKey)
	}
	if _, kept := tenant.ProfileMetadataPatch(map[string]any{PPBApprovalsMetadataKey: []any{}})[PPBApprovalsMetadataKey]; kept {
		t.Fatal("the profile PATCH must ignore ppb_approvals")
	}
	if BusinessTypePharmacy != "pharmacy" {
		t.Fatal("the pharmacy key changed")
	}
	if _, ok := tenant.LookupBusinessType(BusinessTypePharmacy); !ok {
		t.Fatal("pharmacy is not a business type any more")
	}
}

// TestMedicineWordsMatchMarketflowAI checks that every medicine name in marketflow-ai's pharmacy
// blocks is a medicine here too, when a marketflow-ai checkout is found (MARKETFLOW_AI_DIR, or a
// marketflow-ai directory next to this repository). Skipped otherwise, as in CI.
func TestMedicineWordsMatchMarketflowAI(t *testing.T) {
	path := marketflowAIFile(filepath.Join("internal", "compliance", "data", "business_types.yaml"))
	if path == "" {
		t.Skip("no marketflow-ai checkout; set MARKETFLOW_AI_DIR to compare the medicine words")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := medicineWordsSource(raw)
	if src == "" {
		t.Fatal("the pharmacy discount block was not found in marketflow-ai's business types")
	}
	plain := regexp.MustCompile(`^[a-z]{3,}$`)
	// Words that are only the second half of a phrase there ("vitamin C gummies", "cough drops").
	phraseTail := map[string]bool{"gummies": true, "drops": true, "capsules": true}
	checked := 0
	for _, w := range strings.Split(src, "|") {
		if !plain.MatchString(w) || phraseTail[w] {
			continue // a regex form such as medicines? or cough\s+(...)
		}
		checked++
		// A supplement word is known here too, it just is not held (owner, 2026-11-14).
		if !medicineNameRe.MatchString(w) && !medicineFormRe.MatchString(w) && !supplementRe.MatchString(w) {
			t.Errorf("marketflow-ai treats %q as a medicine but this package does not know it", w)
		}
	}
	if checked < 20 {
		t.Fatalf("only %d medicine names found; the block's layout changed", checked)
	}
}

// medicineWordsSource is the medicine list of the pharmacy discount block: the group after the
// first {0,40}?\b(?P<med> up to the next )\b|.
func medicineWordsSource(raw []byte) string {
	s := string(raw)
	i := strings.Index(s, "reason: discounts, offers or giveaways on medicines")
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(s[:i], "- pattern:")
	if start < 0 {
		return ""
	}
	block := s[start:i]
	const open = `{0,40}?\b(?P<med>`
	from := strings.Index(block, open)
	if from < 0 {
		return ""
	}
	rest := block[from+len(open):]
	if to := strings.Index(rest, `)\b|`); to >= 0 {
		return rest[:to]
	}
	return rest
}

func marketflowAIFile(rel string) string {
	var dirs []string
	if d := os.Getenv("MARKETFLOW_AI_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	// This package sits at internal/modules/creative; the repository root is three levels up.
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, filepath.Join(wd, "..", "..", "..", "..", "marketflow-ai"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
