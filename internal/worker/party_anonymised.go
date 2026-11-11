package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
)

// partyAnonymisedSubject is maskani's erasure event: a person's details were erased there and
// maskani dropped its link to the CRM contact. Payload {party_id, crm_contact_id, tenant_id}.
const partyAnonymisedSubject = "maskani.privacy.party_anonymised"

// crmLinkTimeout bounds each service's "is this contact referenced" answer.
const crmLinkTimeout = 3 * time.Second

// finalFailureWriteTimeout bounds the audit write made when an erasure event is given up on.
const finalFailureWriteTimeout = 5 * time.Second

// defaultCRMLinkServices are the services asked before an erased contact is deleted, by their
// in-cluster names (never the public hosts). MF_WORKER_CRM_LINK_SERVICES overrides the list.
const defaultCRMLinkServices = "pos=http://pos-api.pos.svc.cluster.local:4000," +
	"treasury=http://treasury-api.treasury.svc.cluster.local:4000," +
	"library=http://library-api.library.svc.cluster.local:4010," +
	"hospital=http://hospital-api.hospital.svc.cluster.local:4200," +
	"ordering=http://ordering-backend.ordering.svc.cluster.local:4000," +
	"ticketing=http://ticketing-api.ticketing.svc.cluster.local:4009," +
	// projects-api keeps crm_contact_id in project commercial metadata and has no endpoint yet:
	// its 404 counts as "cannot answer", so contacts are stripped, never deleted, until it does.
	"projects=http://projects-api.projects.svc.cluster.local:4000"

// linkService is one service that may hold a crm_contact_id.
type linkService struct {
	Name    string
	BaseURL string
}

// parseLinkServices reads "name=url,name=url"; entries without a name or url are skipped.
func parseLinkServices(s string) []linkService {
	var out []linkService
	for _, part := range strings.Split(s, ",") {
		name, url, ok := strings.Cut(strings.TrimSpace(part), "=")
		name, url = strings.TrimSpace(name), strings.TrimRight(strings.TrimSpace(url), "/")
		if !ok || name == "" || url == "" {
			continue
		}
		out = append(out, linkService{Name: name, BaseURL: url})
	}
	return out
}

// linkChecker returns why a contact must be kept: one short reason per service that still links
// it or could not answer. An empty answer means every service said "not referenced".
type linkChecker interface {
	Blockers(ctx context.Context, tenantID, contactID uuid.UUID) []string
}

// crmLinkChecker asks every configured service's GET /api/v1/s2s/{tenant}/crm-links/{contact}
// in parallel, each within its own time limit, with the shared service key.
type crmLinkChecker struct {
	services []linkService
	key      string
	hc       *http.Client
	timeout  time.Duration
}

func (c *crmLinkChecker) Blockers(ctx context.Context, tenantID, contactID uuid.UUID) []string {
	if len(c.services) == 0 {
		return []string{"no services configured"}
	}
	reasons := make([]string, len(c.services))
	var wg sync.WaitGroup
	for i, svc := range c.services {
		wg.Add(1)
		go func(i int, svc linkService) {
			defer wg.Done()
			if r := c.ask(ctx, svc, tenantID, contactID); r != "" {
				reasons[i] = svc.Name + ": " + r
			}
		}(i, svc)
	}
	wg.Wait()
	out := reasons[:0]
	for _, r := range reasons {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// ask returns "" when the service answered "not referenced", else the reason to keep the contact.
func (c *crmLinkChecker) ask(ctx context.Context, svc linkService, tenantID, contactID uuid.UUID) string {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	url := fmt.Sprintf("%s/api/v1/s2s/%s/crm-links/%s", strings.TrimRight(svc.BaseURL, "/"), tenantID, contactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "bad url"
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "no answer"
	}
	defer resp.Body.Close()
	// 404 included: a service without the endpoint cannot vouch for the contact.
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("http %d", resp.StatusCode)
	}
	var body struct {
		Referenced *bool `json:"referenced"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Referenced == nil {
		return "bad answer"
	}
	if *body.Referenced {
		return "referenced"
	}
	return ""
}

// partyContacts is the part of the contacts service the handler uses.
type partyContacts interface {
	Exists(ctx context.Context, tenantID, contactID uuid.UUID) (bool, error)
	DeleteErased(ctx context.Context, tenantID, contactID uuid.UUID, rec contacts.ErasureRecord) (bool, error)
	Erase(ctx context.Context, tenantID, contactID uuid.UUID, rec contacts.ErasureRecord) (bool, error)
	RecordErasureFailure(ctx context.Context, tenantID, contactID uuid.UUID, rec contacts.ErasureRecord, reason string) error
}

// newLinkHTTPClient never follows a redirect: Go would resend X-API-Key to wherever it points.
// A 3xx is just a non-200 answer, so the contact is stripped.
func newLinkHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// PartyAnonymisedHandler acts on maskani's privacy.party_anonymised (owner decision 2026-11-11):
// the CRM contact is deleted only when every other service says it no longer links it; otherwise
// it is stripped to a minimal record ("Erased", no email, phone or other personal fields, same
// id) and the reasons are kept in its metadata and a tenant audit row. Either way everything else
// marketflow holds about the person (lead, deals, activities, tasks, custom fields, chats,
// meetings, nurture enrollments) is scrubbed or deleted with it. A service that errors, times out, redirects
// or has no endpoint counts as still linking it. Deleting publishes crm.contact.deleted.
// Redelivery is safe: a deleted contact is a no-op, and an erased one is left as it is unless
// every service now lets it go. An event given up on after the last delivery leaves a
// contact.erasure_failed audit row.
type PartyAnonymisedHandler struct {
	contacts partyContacts
	links    linkChecker
	logger   *zap.Logger
}

// NewPartyAnonymisedHandler builds the handler. services is "name=url,..." (empty uses the
// in-cluster defaults); key is the shared service key.
func NewPartyAnonymisedHandler(db *ent.Client, services, key string, logger *zap.Logger) *PartyAnonymisedHandler {
	if strings.TrimSpace(services) == "" {
		services = defaultCRMLinkServices
	}
	return &PartyAnonymisedHandler{
		contacts: contacts.NewService(db, logger),
		links:    &crmLinkChecker{services: parseLinkServices(services), key: key, hc: newLinkHTTPClient(), timeout: crmLinkTimeout},
		logger:   logger.Named("privacy-party-anonymised"),
	}
}

func (*PartyAnonymisedHandler) Subject() string { return partyAnonymisedSubject }
func (*PartyAnonymisedHandler) Durable() string {
	return "marketflow-worker-privacy-party-anonymised"
}

// partyEventIDs are the ids an event carries.
type partyEventIDs struct {
	tenantID, contactID uuid.UUID
	partyID             string
}

// decodePartyEvent reads the ids; an error means the event can never succeed.
func decodePartyEvent(data []byte) (partyEventIDs, error) {
	var ev struct {
		TenantID     string `json:"tenant_id"`
		PartyID      string `json:"party_id"`
		CRMContactID string `json:"crm_contact_id"`
	}
	if err := json.Unmarshal(eventBody(data), &ev); err != nil {
		return partyEventIDs{}, err
	}
	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil || tenantID == uuid.Nil {
		return partyEventIDs{}, fmt.Errorf("bad tenant_id %q", ev.TenantID)
	}
	contactID, err := uuid.Parse(ev.CRMContactID)
	if err != nil || contactID == uuid.Nil {
		return partyEventIDs{}, errors.New("missing or invalid crm_contact_id")
	}
	return partyEventIDs{tenantID: tenantID, contactID: contactID, partyID: ev.PartyID}, nil
}

// OnFinalFailure keeps the erasure request when the registry gives up on it after the last
// delivery, as a contact.erasure_failed audit row, so it can be found and replayed.
func (h *PartyAnonymisedHandler) OnFinalFailure(ctx context.Context, msg jetstream.Msg) {
	ids, err := decodePartyEvent(msg.Data())
	if err != nil {
		return
	}
	rec := contacts.ErasureRecord{Source: "privacy.party_anonymised", SourceID: ids.partyID}
	// Detached from the consumer's context (it may be ending) but bounded, so a stuck database
	// cannot hold the consumer callback.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFailureWriteTimeout)
	defer cancel()
	if err := h.contacts.RecordErasureFailure(wctx, ids.tenantID, ids.contactID, rec,
		"gave up after the last delivery"); err != nil {
		h.logger.Error("erasure failure not recorded", zap.String("tenant_id", ids.tenantID.String()),
			zap.String("contact_id", ids.contactID.String()), zap.Error(err))
	}
}

func (h *PartyAnonymisedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	ev, err := decodePartyEvent(msg.Data())
	if err != nil {
		return dropPoison(h.logger, msg, "decode privacy.party_anonymised", err)
	}
	tenantID, contactID := ev.tenantID, ev.contactID
	// Ids only in every log line below; never the person's details.
	ids := []zap.Field{zap.String("tenant_id", tenantID.String()), zap.String("contact_id", contactID.String()),
		zap.String("party_id", ev.partyID)}

	exists, err := h.contacts.Exists(ctx, tenantID, contactID)
	if err != nil {
		return err
	}
	if !exists {
		h.logger.Info("erased party's contact already gone", ids...)
		return nil
	}

	rec := contacts.ErasureRecord{Source: "privacy.party_anonymised", SourceID: ev.partyID}
	if rec.Reasons = h.links.Blockers(ctx, tenantID, contactID); len(rec.Reasons) == 0 {
		// The contact foreign keys are ON DELETE SET NULL, so nothing in marketflow blocks this;
		// the person's own records go in the same transaction instead of being left unlinked.
		if _, err := h.contacts.DeleteErased(ctx, tenantID, contactID, rec); err != nil {
			return err
		}
		h.logger.Info("erased party's contact deleted; no service links it", ids...)
		return nil
	}
	changed, err := h.contacts.Erase(ctx, tenantID, contactID, rec)
	if err != nil {
		return err
	}
	h.logger.Info("erased party's contact stripped instead of deleted",
		append(ids, zap.Strings("reasons", rec.Reasons), zap.Bool("changed", changed))...)
	return nil
}
