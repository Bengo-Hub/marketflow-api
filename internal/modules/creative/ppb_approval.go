package creative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// PPB advert approval (owner decision 2026-11-14). The Pharmacy and Poisons Act s. 36 and rule 22
// of L.N. 98 of 2022 say no medicine advert runs without the Board's written approval, and printed
// adverts carry the approval reference. For a tenant whose business type is pharmacy, an ad that
// promotes a medicine stays a draft until the owner enters the PPB approval reference for that
// product: it cannot be approved (so it cannot be posted or boosted, both need an approved
// version) and a video ad cannot be rendered. The reference is kept per product in the tenant's
// metadata (ppb_approvals), shown on the ad (a video ad's end card and a poster's subline say
// "PPB approval ref. ...", and an approved version carries metadata.ppb_approval.caption_line for
// the caption) and in the Studio.
//
// Products that are not medicines (lotions, cosmetics, devices, services such as blood pressure
// checks, food and dietary supplements, household "dawa" such as dawa ya meno or dawa ya mbu) are
// not held. Only promotional adverts are held: a version whose words give a price, an offer, a
// call to buy or a use or benefit. A plain availability post ("Panadol in stock", "We are open
// till 9pm") is not held (owner decisions 2026-11-14). Nothing changes for any other business
// type: the platform owner and codevertex tenants are never pharmacies.

const (
	// BusinessTypePharmacy is the stored business type of a pharmacy (tenant.BusinessTypes).
	BusinessTypePharmacy = "pharmacy"
	// PPBApprovalsMetadataKey holds the references: a list of PPBApproval. Only the endpoints
	// below write it; the profile PATCH treats it as reserved (tenant.ReservedMetadataKeys).
	PPBApprovalsMetadataKey = "ppb_approvals"
	// MetaPPBApproval is the version metadata key with the reference the ad was made or approved with.
	MetaPPBApproval = "ppb_approval"

	maxPPBApprovals     = 500
	maxPPBOfferingKey   = 200
	maxPPBOfferingName  = 160
	ppbCaptionPrefix    = "PPB approval ref. "
	errCodePPBRequired  = "ppb_approval_required"
	errCodePPBReference = "invalid_ppb_reference"
)

// ErrPPBApprovalRequired holds a pharmacy's medicine ad until its PPB approval reference is entered.
var ErrPPBApprovalRequired = errors.New("PPB approval reference required")

// PPBApprovalRequiredError names the product whose reference is missing. errors.Is matches
// ErrPPBApprovalRequired.
type PPBApprovalRequiredError struct {
	OfferingKey  string
	OfferingName string
}

func (e *PPBApprovalRequiredError) Error() string {
	return fmt.Sprintf("%s for %q", ErrPPBApprovalRequired, e.OfferingName)
}

// Is makes errors.Is(err, ErrPPBApprovalRequired) true.
func (e *PPBApprovalRequiredError) Is(target error) bool { return target == ErrPPBApprovalRequired }

// PPBApproval is one product's advert approval reference.
type PPBApproval struct {
	OfferingKey  string `json:"offering_key"`
	OfferingName string `json:"offering_name,omitempty"`
	Reference    string `json:"reference"`
	EnteredBy    string `json:"entered_by,omitempty"`
	EnteredAt    string `json:"entered_at"`
}

// PPBStatus is what the Studio shows for one ad.
type PPBStatus struct {
	// Pharmacy is true when the tenant's business type is pharmacy.
	Pharmacy bool `json:"pharmacy"`
	// Medicine is true when the ad's product reads as a medicine.
	Medicine bool `json:"medicine"`
	// Required is true when the ad's product needs a reference (pharmacy and medicine); its
	// promotional versions are held until one is entered, availability posts are not.
	Required     bool         `json:"required"`
	OfferingKey  string       `json:"offering_key,omitempty"`
	OfferingName string       `json:"offering_name,omitempty"`
	Approval     *PPBApproval `json:"approval,omitempty"`
	// CaptionLine is the line the ad carries ("PPB approval ref. ..."), set once a reference exists.
	CaptionLine string `json:"caption_line,omitempty"`
}

// Held reports whether the ad must stay a draft.
func (st PPBStatus) Held() bool { return st.Required && st.Approval == nil }

// medicineNameRe mirrors the medicine words of marketflow-ai's pharmacy blocks
// (internal/compliance/data/business_types.yaml: the prescription-only list and the general sale
// medicines). TestMedicineWordsMatchMarketflowAI compares the names with that file when a checkout
// is at hand. A product named by one of these is a medicine.
var medicineNameRe = regexp.MustCompile(`(?i)\b(` +
	// medicine categories and general sale medicines
	`medicines?|medications?|drugs|painkillers?|pain\s+relief|eye\s+drops|ear\s+drops|nasal\s+drops|paracetamol|panadol|calpol|ibuprofen|brufen|aspirin|diclofenac|antacids?|eno|gaviscon|cough\s+(syrups?|mixtures?|drops|medicines?)|lozenges|strepsils|ors|oral\s+rehydration(\s+salts?)?|anti-?malarials?|coartem|artemether|antihistamines?|piriton|cetirizine|loratadine|de-?wormers?|albendazole|mebendazole|dawa|` +
	// prescription-only and controlled medicines
	`antibiotics?|amoxicillin|amoxil|augmentin|azithromycin|ciprofloxacin|doxycycline|metronidazole|flagyl|ozempic|semaglutide|wegovy|rybelsus|mounjaro|zepbound|tirzepatide|saxenda|liraglutide|(weight[\s-]loss|slimming)\s+(injections?|jabs?|pens?)|tramadol|codeine|diazepam|valium|alprazolam|xanax|morphine|pethidine|sildenafil|viagra|tadalafil|cialis|misoprostol|cytotec|clomiphene|clomid` +
	`)\b`)

// medicineFormRe is a strength or dosage form in a catalogue name ("500mg", "tablets", "syrup").
// It marks a medicine unless the product is a supplement.
var medicineFormRe = regexp.MustCompile(`(?i)\b(\d+(\.\d+)?\s*(mg|mcg|iu)|tablets?|tabs|capsules?|caplets?|syrups?|suspension|injections?|inhalers?|pessaries|suppositor(y|ies)|drops)\b`)

// supplementRe marks a food or dietary supplement. Owner decision 2026-11-14: supplements (Omega 3,
// multivitamins, vitamin C and similar) are not held for a PPB reference, although the discount
// rules on medicines still cover them in marketflow-ai.
var supplementRe = regexp.MustCompile(`(?i)\b(supplements?|nutritional|dietary|multi-?vitamins?|vitamins?|omega[\s-]?3|fish\s+oil|cod\s+liver\s+oil|probiotics?|collagen|biotin|calcium|magnesium|zinc|iron|folic\s+acid|ginseng|spirulina|moringa|immune\s+boosters?|seven\s+seas|centrum|wellman|wellwoman|pregnacare|vitabiotics|supradyn)\b`)

// householdDawaRe is "dawa" that is not a medicine (owner decision 2026-11-14): toothpaste, mosquito
// and insect repellent, hair and cleaning products. The same list is the exempt pattern of
// marketflow-ai's pharmacy discount block.
var householdDawaRe = regexp.MustCompile(`(?i)\bdawa\s+(ya|za)\s+(meno|mbu|nywele|kuosha|kufulia|wadudu|mende|panya|kunguni|viroboto|chawa|kusafisha|nguo|vyombo|choo|sakafu|mimea|shamba|mifugo|kupaka|nyumba)\b`)

// IsMedicineOffering reports whether an ad's product reads as a medicine: the offering's name,
// category and description, or the ad's title when no offering was picked. A service (a free
// blood pressure check, a consultation), a supplement and household "dawa" are never a medicine.
func IsMedicineOffering(offering map[string]any, title string) bool {
	if strings.EqualFold(strings.TrimSpace(str(offering["type"])), "SERVICE") {
		return false
	}
	parts := []string{str(offering["name"]), str(offering["category"]), str(offering["category_name"]), str(offering["description"])}
	if strings.TrimSpace(str(offering["name"])) == "" {
		parts = append(parts, title)
	}
	text := householdDawaRe.ReplaceAllString(strings.Join(parts, "\n"), " ")
	if medicineNameRe.MatchString(text) {
		return true
	}
	return medicineFormRe.MatchString(text) && !supplementRe.MatchString(text)
}

// promotionalRe marks ad words that promote a product (owner decision 2026-11-14): a price, an
// offer, a call to buy, or a use or benefit. A plain availability post ("Panadol in stock", "Now
// stocking Coartem", "We are open till 9pm") has none of them and is not held.
var promotionalRe = regexp.MustCompile(`(?i)\b(` +
	// price
	`(kes|ksh|kshs|sh|shs)\.?\s*\d|\d[\d,]*\s*bob|prices?|bei|` +
	// offer
	`\d+\s*%\s*(off|discount)|discount\w*|offers?|deals?|promos?|promotions?|sale|save|free|cheap\w*|half\s+price|punguzo|ofa|zawadi|bure|` +
	// call to buy
	`buy|order\w*|shop\s+now|get\s+(yours|it|one)|grab|nunua|agiza|pata\s+yako|(dm|inbox)\s+(us|to)|` +
	// use or benefit
	`relie(f|ves?|ving)|helps|treats?|treatment|soothes?|cures?|heals?|fast[\s-]acting|effective|works?|fights?|protects?|boosts?|feel\s+better|get\s+well|beats?|kills?|recover\w*|symptoms?|best|tiba|inatibu|husaidia|kupona|maumivu|homa|` +
	`for\s+(pain|fever|headaches?|colds?|flu|coughs?|allerg\w+|malaria|heartburn|acidity|diarrh\w+|ulcers?|infections?|stomach\w*)` +
	`)\b`)

// ppbLineRe is the reference line MarketFlow adds itself; it never makes an ad promotional.
var ppbLineRe = regexp.MustCompile(`(?i)PPB approval ref\.\s*\S+`)

// IsPromotionalText reports whether the words of an ad promote the product (promotionalRe). Ad
// words with nothing in them, or availability wording only, are not promotional.
func IsPromotionalText(texts ...string) bool {
	return promotionalRe.MatchString(ppbLineRe.ReplaceAllString(strings.Join(texts, "\n"), " "))
}

// versionPromotes reports whether a version's words promote the product: the poster's or end
// card's headline, subline and call to action, a price, and a video ad's script.
func versionPromotes(v *ent.CreativeVersion) bool {
	if v == nil {
		return false
	}
	texts := []string{str(v.Copy["headline"]), str(v.Copy["subline"]), str(v.Copy["cta"])}
	if strings.TrimSpace(str(v.Copy["price"])) != "" {
		return true
	}
	if card, ok := v.Copy["end_card"].(map[string]any); ok && strings.TrimSpace(str(card["price"])) != "" {
		return true
	}
	texts = append(texts, scriptText(v.Copy)...)
	return IsPromotionalText(texts...)
}

// OfferingKey is the key a product's reference is stored under: source:source_id for a catalogue
// item, else name:<the name in lower case>, the ad's title when there is no offering. Empty when
// there is nothing to name the product by.
func OfferingKey(offering map[string]any, title string) (key, name string) {
	name = strings.TrimSpace(str(offering["name"]))
	if name == "" {
		name = strings.TrimSpace(title)
	}
	if id := strings.TrimSpace(str(offering["source_id"])); id != "" {
		src := strings.ToLower(strings.TrimSpace(str(offering["source"])))
		if src == "" {
			src = "offering"
		}
		return cutKey(src + ":" + id), name
	}
	n := strings.Join(strings.Fields(strings.ToLower(name)), " ")
	if n == "" {
		return "", name
	}
	return cutKey("name:" + n), name
}

// cutKey keeps a key within maxPPBOfferingKey bytes without splitting a character.
func cutKey(k string) string {
	if len(k) <= maxPPBOfferingKey {
		return k
	}
	k = k[:maxPPBOfferingKey]
	for len(k) > 0 && !utf8.ValidString(k) {
		k = k[:len(k)-1]
	}
	return k
}

// ReadPPBApprovals returns the references stored in tenant metadata; bad entries are skipped.
func ReadPPBApprovals(meta map[string]any) []PPBApproval {
	raw, ok := meta[PPBApprovalsMetadataKey]
	if !ok || raw == nil {
		return []PPBApproval{}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return []PPBApproval{}
	}
	var list []PPBApproval
	if json.Unmarshal(b, &list) != nil {
		return []PPBApproval{}
	}
	out := make([]PPBApproval, 0, len(list))
	for _, a := range list {
		if a.OfferingKey != "" && strings.TrimSpace(a.Reference) != "" {
			out = append(out, a)
		}
	}
	return out
}

func findPPBApproval(meta map[string]any, key string) *PPBApproval {
	if key == "" {
		return nil
	}
	for _, a := range ReadPPBApprovals(meta) {
		if a.OfferingKey == key {
			a := a
			return &a
		}
	}
	return nil
}

// ppbReferenceRe is what a reference may look like: letters, digits and the separators a Board
// reference uses (PPB/ADV/2025/0123), 3 to 80 characters.
var ppbReferenceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 /._#()-]{2,79}$`)

// NormalizePPBReference trims a reference and checks its form.
func NormalizePPBReference(ref string) (string, error) {
	ref = strings.Join(strings.Fields(ref), " ")
	if !ppbReferenceRe.MatchString(ref) {
		return "", invalid("the PPB approval reference must be 3 to 80 letters, digits, spaces or / . _ # ( ) -")
	}
	return ref, nil
}

// PPBCaptionLine is the line an ad carries for its reference.
func PPBCaptionLine(ref string) string { return ppbCaptionPrefix + ref }

// withPPBLine adds the reference line to a line of ad text (a poster's subline, a video ad's end
// card subline), once, within the length limit.
func withPPBLine(line, ref string) string {
	add := PPBCaptionLine(ref)
	line = strings.TrimSpace(line)
	if strings.Contains(line, ref) {
		return line
	}
	if line == "" {
		return add
	}
	sep := ". "
	if strings.ContainsAny(line[len(line)-1:], ".!?") {
		sep = " "
	}
	if room := maxCopyLen - len(sep) - len(add); len(line) > room {
		if room <= 0 {
			return add
		}
		line = line[:room]
		for len(line) > 0 && !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
		line = strings.TrimSpace(line)
	}
	return line + sep + add
}

// ppbStatus works out the hold for a brief from the tenant row.
func ppbStatus(t *ent.Tenant, b *ent.CreativeBrief) PPBStatus {
	var st PPBStatus
	if t == nil || b == nil {
		return st
	}
	st.Pharmacy = strings.EqualFold(strings.TrimSpace(t.BusinessType), BusinessTypePharmacy)
	st.Medicine = IsMedicineOffering(b.Offering, b.Title)
	st.OfferingKey, st.OfferingName = OfferingKey(b.Offering, b.Title)
	st.Required = st.Pharmacy && st.Medicine
	if st.Required {
		if a := findPPBApproval(t.Metadata, st.OfferingKey); a != nil {
			st.Approval = a
			st.CaptionLine = PPBCaptionLine(a.Reference)
		}
	}
	return st
}

// PPBStatusForBrief returns the hold for one of the tenant's ads.
func (s *Service) PPBStatusForBrief(ctx context.Context, tenantID, briefID uuid.UUID) (PPBStatus, error) {
	b, err := s.brief(ctx, tenantID, briefID)
	if err != nil {
		return PPBStatus{}, err
	}
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		if ent.IsNotFound(err) {
			return PPBStatus{}, nil // no tenant row: not a pharmacy
		}
		return PPBStatus{}, err
	}
	return ppbStatus(t, b), nil
}

// CheckPPBApproval returns a *PPBApprovalRequiredError while the ad's product needs a reference
// that is missing, whatever a version says. CheckPPBApprovalForVersion is the check that holds a
// version: it also lets an availability post through.
func (s *Service) CheckPPBApproval(ctx context.Context, tenantID, briefID uuid.UUID) (PPBStatus, error) {
	st, err := s.PPBStatusForBrief(ctx, tenantID, briefID)
	if err != nil {
		return st, err
	}
	if st.Held() {
		return st, &PPBApprovalRequiredError{OfferingKey: st.OfferingKey, OfferingName: st.OfferingName}
	}
	return st, nil
}

// CheckPPBApprovalForVersion returns a *PPBApprovalRequiredError while the version is held: a
// pharmacy's medicine ad whose words promote the product (a price, an offer, a call to buy, a use
// or benefit) and whose reference is missing. A plain availability post is not held (owner
// decision 2026-11-14). Other callers (the autopilot pipeline) can use it before they spend
// anything on a version.
func (s *Service) CheckPPBApprovalForVersion(ctx context.Context, tenantID uuid.UUID, v *ent.CreativeVersion) (PPBStatus, error) {
	st, err := s.PPBStatusForBrief(ctx, tenantID, v.BriefID)
	if err != nil {
		return st, err
	}
	if st.Held() && versionPromotes(v) {
		return st, &PPBApprovalRequiredError{OfferingKey: st.OfferingKey, OfferingName: st.OfferingName}
	}
	return st, nil
}

// ppbHold is decideVia's check: approving a held ad fails, and an ad that needs a reference
// returns it so the approval records it. Anything but an approval passes.
func (s *Service) ppbHold(ctx context.Context, tenantID, versionID uuid.UUID, status string) (*PPBApproval, error) {
	if status != StatusApproved {
		return nil, nil
	}
	v, err := s.db.CreativeVersion.Query().Where(creativeversion.ID(versionID), creativeversion.TenantID(tenantID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil // decideVia answers a missing version itself
	}
	if err != nil {
		return nil, err
	}
	st, err := s.CheckPPBApprovalForVersion(ctx, tenantID, v)
	if err != nil {
		return nil, err
	}
	return st.Approval, nil
}

// stampPPBApproval records the reference an approved version was approved with.
func stampPPBApproval(ctx context.Context, tx *ent.Tx, versionID uuid.UUID, a *PPBApproval) error {
	if a == nil {
		return nil
	}
	v, err := tx.CreativeVersion.Get(ctx, versionID)
	if err != nil {
		return err
	}
	meta := copyMap(v.Metadata)
	meta[MetaPPBApproval] = ppbVersionMeta(*a)
	return tx.CreativeVersion.UpdateOneID(versionID).SetMetadata(meta).Exec(ctx)
}

func ppbVersionMeta(a PPBApproval) map[string]any {
	return map[string]any{"reference": a.Reference, "offering_key": a.OfferingKey, "caption_line": PPBCaptionLine(a.Reference)}
}

// ppbForRender is RenderVideoAd's check: a held video ad is not rendered, and one with a reference
// gets the reference line on its end card. It returns the copy to save (nil when unchanged) and
// writes the reference into meta.
func (s *Service) ppbForRender(ctx context.Context, tenantID uuid.UUID, v *ent.CreativeVersion, meta map[string]any) (map[string]any, error) {
	st, err := s.CheckPPBApprovalForVersion(ctx, tenantID, v)
	if err != nil || st.Approval == nil {
		return nil, err
	}
	meta[MetaPPBApproval] = ppbVersionMeta(*st.Approval)
	card, _ := v.Copy["end_card"].(map[string]any)
	card = copyMap(card)
	line := withPPBLine(str(card["subline"]), st.Approval.Reference)
	if line == str(card["subline"]) {
		return nil, nil
	}
	card["subline"] = line
	copyFields := copyMap(v.Copy)
	copyFields["end_card"] = card
	return copyFields, nil
}

// ppbPosterLine puts the reference on a poster's subline when the ad needs one and it is known.
// A poster draft is still made without it (drafts are allowed); approval waits for the reference.
func (s *Service) ppbPosterLine(ctx context.Context, tenantID, briefID uuid.UUID, subline string) string {
	st, err := s.PPBStatusForBrief(ctx, tenantID, briefID)
	if err != nil || st.Approval == nil {
		return subline
	}
	return withPPBLine(subline, st.Approval.Reference)
}

// ListPPBApprovals returns the tenant's references.
func (s *Service) ListPPBApprovals(ctx context.Context, tenantID uuid.UUID) ([]PPBApproval, bool, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		if ent.IsNotFound(err) {
			return []PPBApproval{}, false, nil
		}
		return nil, false, err
	}
	return ReadPPBApprovals(t.Metadata), strings.EqualFold(strings.TrimSpace(t.BusinessType), BusinessTypePharmacy), nil
}

// SetPPBApproval saves or replaces one product's reference; an empty reference removes it. The
// list is changed in one UPDATE that keeps every other entry, so two owners saving different
// products never lose one.
func (s *Service) SetPPBApproval(ctx context.Context, tenantID uuid.UUID, key, name, reference string, by *uuid.UUID) (*PPBApproval, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxPPBOfferingKey {
		return nil, invalid(fmt.Sprintf("offering_key is required (max %d characters)", maxPPBOfferingKey))
	}
	var entry *PPBApproval
	if strings.TrimSpace(reference) != "" {
		ref, err := NormalizePPBReference(reference)
		if err != nil {
			return nil, err
		}
		entry = &PPBApproval{OfferingKey: key, OfferingName: excerpt(strings.TrimSpace(name), maxPPBOfferingName),
			Reference: ref, EnteredAt: s.nowFn().UTC().Format(time.RFC3339)}
		if by != nil {
			entry.EnteredBy = by.String()
		}
		t, err := s.db.Tenant.Get(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if list := ReadPPBApprovals(t.Metadata); len(list) >= maxPPBApprovals && findPPBApproval(t.Metadata, key) == nil {
			return nil, invalid(fmt.Sprintf("at most %d PPB approval references", maxPPBApprovals))
		}
	}
	var add []byte
	if entry != nil {
		var err error
		if add, err = json.Marshal([]PPBApproval{*entry}); err != nil {
			return nil, err
		}
	}
	col := enttenant.FieldMetadata
	list := "COALESCE(" + col + "->'" + PPBApprovalsMetadataKey + "', '[]'::jsonb)"
	n, err := s.db.Tenant.Update().
		Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(col, sql.ExprFunc(func(b *sql.Builder) {
				b.WriteString("jsonb_set(COALESCE(" + col + ", '{}'::jsonb), '{" + PPBApprovalsMetadataKey + "}', " +
					"(SELECT COALESCE(jsonb_agg(e), '[]'::jsonb) FROM jsonb_array_elements(" + list + ") AS e WHERE e->>'offering_key' <> ")
				b.Arg(key)
				b.WriteString(")")
				if add != nil {
					b.WriteString(" || ")
					b.Arg(string(add))
					b.WriteString("::jsonb")
				}
				b.WriteString(")")
			}))
		}).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, &ent.NotFoundError{}
	}
	return entry, nil
}

// writePPBRequired answers 409 ppb_approval_required (handler.fail).
func writePPBRequired(w http.ResponseWriter, err error) {
	details := map[string]any{"setting": PPBApprovalsMetadataKey}
	name := "this product"
	var held *PPBApprovalRequiredError
	if errors.As(err, &held) {
		details["offering_key"], details["offering_name"] = held.OfferingKey, held.OfferingName
		if held.OfferingName != "" {
			name = held.OfferingName
		}
	}
	httpapi.Error(w, http.StatusConflict, errCodePPBRequired,
		fmt.Sprintf("This ad promotes a medicine (%s). The Pharmacy and Poisons Board must approve every medicine advert before it runs, so it stays a draft until you enter the PPB approval reference for this product in the Studio. A plain availability post (for example \"in stock\") does not need one.", name),
		details)
}

// ppbSetRequest is the body of PUT /creative/ppb-approvals.
type ppbSetRequest struct {
	OfferingKey  string `json:"offering_key"`
	OfferingName string `json:"offering_name"`
	// Reference is the Board's advert approval reference; empty removes the product's reference.
	Reference string `json:"reference"`
}

// ListPPBApprovals handles GET /api/v1/creative/ppb-approvals: the business's PPB advert approval
// references, one per product.
// @Summary List PPB advert approval references (pharmacies)
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /creative/ppb-approvals [get]
func (h *Handler) ListPPBApprovals(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	list, pharmacy, err := h.svc.ListPPBApprovals(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"approvals": list, "pharmacy": pharmacy})
}

// SetPPBApproval handles PUT /api/v1/creative/ppb-approvals: saves the PPB advert approval
// reference of one product (owner or admin). An empty reference removes it.
// @Summary Save a product's PPB advert approval reference (owner or admin)
// @Tags Creative
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body ppbSetRequest true "Product and reference"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 403 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/ppb-approvals [put]
func (h *Handler) SetPPBApproval(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if h.canApprove != nil && !h.canApprove(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "approval_not_allowed", "only the business owner or an admin can enter a PPB approval reference", nil)
		return
	}
	var req ppbSetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	a, err := h.svc.SetPPBApproval(r.Context(), tid, req.OfferingKey, req.OfferingName, req.Reference, userOf(r))
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			httpapi.Error(w, http.StatusBadRequest, errCodePPBReference, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "), nil)
			return
		}
		h.fail(w, err)
		return
	}
	event := "creative.ppb_approval_set"
	payload := map[string]any{"offering_key": strings.TrimSpace(req.OfferingKey)}
	if a == nil {
		event = "creative.ppb_approval_removed"
	} else {
		payload["reference"] = a.Reference
	}
	audit.Tenant(r.Context(), h.svc.db, h.logger, tid, event, "tenant", &tid, payload)
	h.logger.Info("ppb approval reference saved", zap.String("tenant_id", tid.String()), zap.String("event", event))
	httpapi.JSON(w, http.StatusOK, map[string]any{"approval": a})
}

// BriefPPBApproval handles GET /api/v1/creative/briefs/{id}/ppb-approval: whether the ad needs a
// PPB approval reference, and the one it has.
// @Summary Whether an ad needs a PPB advert approval reference (pharmacies)
// @Tags Creative
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Brief UUID"
// @Success 200 {object} PPBStatus
// @Failure 404 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /creative/briefs/{id}/ppb-approval [get]
func (h *Handler) BriefPPBApproval(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	st, err := h.svc.PPBStatusForBrief(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, st)
}
