package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/reqid"
)

// A credit purchase started by the WhatsApp Buy credits button was paid (Sprint 17). Treasury
// reports every payment on treasury.payment.succeeded; subscriptions-api grants the credits from
// the same event. This consumer passes credit purchases to marketflow-ai, which knows whether one
// came from a WhatsApp tap and resumes what was waiting (a video the person approved starts; a
// plan comes back with its Approve button). marketflow-ai answers 409 until the credits are in
// the balance, and the event is retried (10 s, 1 min, 5 min, 10 min). Nothing here charges.

// WhatsAppTopupPaidHandler subscribes to treasury.payment.succeeded.
type WhatsAppTopupPaidHandler struct {
	aiURL  string
	aiKey  string
	hc     *http.Client
	wa     *creative.PlatformWhatsApp
	logger *zap.Logger
}

// NewWhatsAppTopupPaidHandler builds the consumer; wa sends from the platform number.
func NewWhatsAppTopupPaidHandler(aiURL, aiKey string, wa *creative.PlatformWhatsApp, logger *zap.Logger) *WhatsAppTopupPaidHandler {
	return &WhatsAppTopupPaidHandler{aiURL: strings.TrimSuffix(aiURL, "/"), aiKey: aiKey, hc: reqid.Client(30 * time.Second),
		wa: wa, logger: logger.Named("whatsapp-topup-paid")}
}

func (*WhatsAppTopupPaidHandler) Subject() string { return "treasury.payment.succeeded" }
func (*WhatsAppTopupPaidHandler) Durable() string { return "marketflow-whatsapp-topup-paid" }

// errCreditsNotLanded makes the registry retry the event later.
var errCreditsNotLanded = errors.New("whatsapp top-up: the credits have not landed yet")

// creditPurchaseRef is the reference type of every credit purchase (subscriptions-api).
const creditPurchaseRef = "credit_purchase"

func (h *WhatsAppTopupPaidHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID      string `json:"tenant_id"`
		IntentID      string `json:"intent_id"`
		ReferenceType string `json:"reference_type"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode treasury.payment.succeeded", err)
	}
	if ev.ReferenceType != creditPurchaseRef || ev.TenantID == "" || ev.IntentID == "" || h.aiURL == "" {
		return nil
	}
	reply, err := h.resume(ctx, ev.TenantID, ev.IntentID)
	if err != nil {
		return err
	}
	if reply.To == "" || strings.TrimSpace(reply.Text) == "" || h.wa == nil {
		return nil // not a WhatsApp purchase, or already handled
	}
	return h.wa.SendRich(ctx, reply.To, reply.Text, reply.Interactive, "wa-topup-"+ev.IntentID)
}

// topupReply is marketflow-ai's message for the buyer's number.
type topupReply struct {
	To          string                             `json:"to"`
	Text        string                             `json:"text"`
	Interactive *notifications.WhatsAppInteractive `json:"interactive"`
}

func (h *WhatsAppTopupPaidHandler) resume(ctx context.Context, tenantID, intentID string) (topupReply, error) {
	body, _ := json.Marshal(map[string]string{"tenant_id": tenantID, "intent_id": intentID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.aiURL+"/api/v1/internal/os/whatsapp-topup-paid", bytes.NewReader(body))
	if err != nil {
		return topupReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.aiKey)
	resp, err := h.hc.Do(req)
	if err != nil {
		return topupReply{}, fmt.Errorf("whatsapp-topup-paid: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusConflict:
		return topupReply{}, errCreditsNotLanded
	case resp.StatusCode != http.StatusOK:
		return topupReply{}, fmt.Errorf("whatsapp-topup-paid: status %d", resp.StatusCode)
	}
	var out topupReply
	if err := json.Unmarshal(data, &out); err != nil {
		return topupReply{}, fmt.Errorf("whatsapp-topup-paid: %w", err)
	}
	return out, nil
}
