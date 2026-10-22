package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
)

// metaGraphBase is the Graph API root the enricher calls. Tests point it at a fake server.
const metaGraphBase = "https://graph.facebook.com/v21.0"

// MetaLeadEnricher fills in a Meta Lead Ads lead after the webhook stored it with ids only.
// The webhook must answer Meta fast, so the Graph call happens here, off the request path:
// GET /{leadgen_id}?fields=field_data with the tenant's (decrypted) Page token, then the
// lead's name, email, phone and form answers are set and the Contact is created or found
// (contacts.Service.Create is an atomic create-or-get on email or phone). Marketing consent
// is never switched on here; the contact keeps the KDPA default of opted out.
type MetaLeadEnricher struct {
	db        *ent.Client
	tokens    *MetaTokenResolver
	contacts  *contacts.Service
	hc        *http.Client
	graphBase string
	logger    *zap.Logger
}

func (*MetaLeadEnricher) Subject() string { return "lead.created" }
func (*MetaLeadEnricher) Durable() string { return "marketflow-worker-meta-lead-enricher" }

// errMetaPermanent marks a Graph answer that retrying cannot fix (token revoked, lead gone).
type errMetaPermanent struct{ status int }

func (e errMetaPermanent) Error() string { return fmt.Sprintf("graph answered %d", e.status) }

func (h *MetaLeadEnricher) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodeLeadCreated(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode lead.created", err)
	}
	if ev.Source != "meta_ad" || h.db == nil {
		return nil
	}
	l, err := h.db.Lead.Query().Where(lead.ID(ev.LeadID), lead.TenantID(ev.TenantID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if l.Email != nil || l.Phone != nil {
		return nil // already enriched (redelivery)
	}
	leadgenID, _ := l.Metadata["meta_leadgen_id"].(string)
	if leadgenID == "" {
		return nil
	}
	// The lead came from one Page; read it with that Page's token (any connected Page, not
	// only the default one).
	pageID, _ := l.Metadata["meta_page_id"].(string)
	token, ok := h.tokens.ResolvePage(ctx, ev.TenantID.String(), pageID)
	if !ok {
		h.logger.Warn("meta lead not enriched: tenant has no Page token", zap.String("tenant", ev.TenantID.String()))
		return nil
	}

	fields, err := h.fetch(ctx, leadgenID, token)
	if err != nil {
		var permanent errMetaPermanent
		if errors.As(err, &permanent) {
			h.logger.Warn("meta lead not enriched", zap.String("lead", l.ID.String()), zap.Error(err))
			return nil
		}
		return err // network or 5xx: JetStream redelivers
	}
	return h.apply(ctx, l, parseMetaLeadFields(fields))
}

type metaFieldDatum struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

func (h *MetaLeadEnricher) fetch(ctx context.Context, leadgenID, token string) ([]metaFieldDatum, error) {
	base := h.graphBase
	if base == "" {
		base = metaGraphBase
	}
	u := base + "/" + url.PathEscape(leadgenID) + "?fields=field_data&access_token=" + url.QueryEscape(token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	hc := h.hc
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("graph answered %d", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, errMetaPermanent{status: resp.StatusCode}
	}
	var out struct {
		FieldData []metaFieldDatum `json:"field_data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, errMetaPermanent{status: resp.StatusCode}
	}
	return out.FieldData, nil
}

// metaLeadFields is what a Meta lead form answered.
type metaLeadFields struct {
	FirstName, LastName, Email, Phone string
	Answers                           map[string]any
}

// parseMetaLeadFields maps Meta's field_data. Standard question names fill the lead's own
// fields; every other answer (custom questions, consent checkboxes) is kept as an answer.
func parseMetaLeadFields(data []metaFieldDatum) metaLeadFields {
	out := metaLeadFields{Answers: map[string]any{}}
	for _, d := range data {
		v := ""
		if len(d.Values) > 0 {
			v = strings.TrimSpace(d.Values[0])
		}
		if v == "" {
			continue
		}
		switch strings.ToLower(d.Name) {
		case "first_name":
			out.FirstName = v
		case "last_name":
			out.LastName = v
		case "full_name":
			if out.FirstName == "" && out.LastName == "" {
				parts := strings.Fields(v)
				out.FirstName = parts[0]
				if len(parts) > 1 {
					out.LastName = strings.Join(parts[1:], " ")
				}
			}
		case "email":
			out.Email = v
		case "phone_number", "phone":
			out.Phone = v
		default:
			if len(d.Values) > 1 {
				out.Answers[d.Name] = d.Values
			} else {
				out.Answers[d.Name] = v
			}
		}
	}
	return out
}

func (h *MetaLeadEnricher) apply(ctx context.Context, l *ent.Lead, f metaLeadFields) error {
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	meta := map[string]any{}
	for k, v := range l.Metadata {
		meta[k] = v
	}
	meta["meta_enriched_at"] = time.Now().UTC().Format(time.RFC3339)

	upd := h.db.Lead.UpdateOneID(l.ID).SetMetadata(meta)
	if p := str(f.Email); p != nil {
		upd.SetEmail(*p)
	}
	if p := str(f.Phone); p != nil {
		upd.SetPhone(*p)
	}
	if p := str(f.FirstName); p != nil && l.FirstName == nil {
		upd.SetFirstName(*p)
	}
	if p := str(f.LastName); p != nil && l.LastName == nil {
		upd.SetLastName(*p)
	}
	if len(f.Answers) > 0 {
		answers := map[string]any{}
		for k, v := range l.QualificationAnswers {
			answers[k] = v
		}
		for k, v := range f.Answers {
			answers[k] = v
		}
		upd.SetQualificationAnswers(answers)
	}
	if err := upd.Exec(ctx); err != nil {
		return fmt.Errorf("enrich lead: %w", err)
	}

	if f.Email == "" && f.Phone == "" {
		return nil // nothing to identify a contact by
	}
	leadID := l.ID
	if _, err := h.contacts.Create(ctx, contacts.CreateInput{
		TenantID: l.TenantID, Email: str(f.Email), Phone: str(f.Phone),
		FirstName: str(f.FirstName), LastName: str(f.LastName),
		ContactType: "prospect", LeadID: &leadID,
		Metadata: map[string]any{"source": "meta_lead_ad", "consent_source": "meta_lead_form"},
	}); err != nil {
		h.logger.Warn("meta lead enriched but contact upsert failed", zap.String("lead", l.ID.String()), zap.Error(err))
	}
	return nil
}
