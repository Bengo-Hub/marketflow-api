package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// ── Ordering consumer ──────────────────────────────────────────────────────

// OrderingOrderCreatedHandler subscribes to ordering.order.created.
// When the event contains crm_contact_id, it logs an Activity on the CRM contact.
type OrderingOrderCreatedHandler struct {
	db     *ent.Client
	svc    *activities.Service
	logger *zap.Logger
}

func (h *OrderingOrderCreatedHandler) Subject() string { return "ordering.order.created" }
func (h *OrderingOrderCreatedHandler) Durable() string {
	return "marketflow-worker-crm-ordering-order"
}

func (h *OrderingOrderCreatedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID     string  `json:"tenant_id"`
		OrderID      string  `json:"order_id"`
		OrderNumber  string  `json:"order_number"`
		CRMContactID *string `json:"crm_contact_id"`
		Total        float64 `json:"total"`
		Currency     string  `json:"currency"`
		Status       string  `json:"status"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode ordering.order.created", err)
	}

	if ev.CRMContactID == nil || *ev.CRMContactID == "" {
		return nil // no CRM link — skip
	}
	if h.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad tenant_id", err)
	}
	contactID, err := uuid.Parse(*ev.CRMContactID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad crm_contact_id", err)
	}

	body := fmt.Sprintf("Order %s placed — %s %.2f", ev.OrderNumber, ev.Currency, ev.Total)
	now := time.Now()
	source := "ordering-backend"
	_, err = h.svc.Log(ctx, activities.CreateInput{
		TenantID:    tenantID,
		EntityType:  "contact",
		EntityID:    contactID,
		ActivityType: "order",
		Subject:     &ev.OrderNumber,
		Body:        &body,
		Source:      &source,
		ExternalRef: &ev.OrderID,
		PerformedAt: &now,
	})
	if err != nil {
		h.logger.Error("failed to log ordering activity on contact",
			zap.String("contact_id", contactID.String()),
			zap.Error(err),
		)
		return err
	}
	h.logger.Info("ordering activity logged on CRM contact",
		zap.String("contact_id", contactID.String()),
		zap.String("order_id", ev.OrderID),
	)
	queueConversion(ctx, h.db, h.logger, tenantID, contactID, "ordering:"+ev.OrderID, ev.Total, ev.Currency, "order")
	return nil
}

// ── POS sale consumer ──────────────────────────────────────────────────────

// POSSaleFinalizedHandler subscribes to pos.sale.finalized.
type POSSaleFinalizedHandler struct {
	db     *ent.Client
	svc    *activities.Service
	logger *zap.Logger
}

func (h *POSSaleFinalizedHandler) Subject() string { return "pos.sale.finalized" }
func (h *POSSaleFinalizedHandler) Durable() string { return "marketflow-worker-crm-pos-sale" }

func (h *POSSaleFinalizedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID     string  `json:"tenant_id"`
		OrderID      string  `json:"order_id"`
		OrderNumber  string  `json:"order_number"`
		CRMContactID *string `json:"crm_contact_id"`
		Total        float64 `json:"total"`
		Currency     string  `json:"currency"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode pos.sale.finalized", err)
	}

	if ev.CRMContactID == nil || *ev.CRMContactID == "" {
		return nil
	}
	if h.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad tenant_id", err)
	}
	contactID, err := uuid.Parse(*ev.CRMContactID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad crm_contact_id", err)
	}

	body := fmt.Sprintf("POS sale %s — %s %.2f", ev.OrderNumber, ev.Currency, ev.Total)
	now := time.Now()
	source := "pos-api"
	_, err = h.svc.Log(ctx, activities.CreateInput{
		TenantID:    tenantID,
		EntityType:  "contact",
		EntityID:    contactID,
		ActivityType: "order",
		Subject:     &ev.OrderNumber,
		Body:        &body,
		Source:      &source,
		ExternalRef: &ev.OrderID,
		PerformedAt: &now,
	})
	if err != nil {
		h.logger.Error("failed to log POS sale activity",
			zap.String("contact_id", contactID.String()), zap.Error(err))
		return err
	}
	queueConversion(ctx, h.db, h.logger, tenantID, contactID, "pos:"+ev.OrderID, ev.Total, ev.Currency, "order")
	return nil
}

// ── POS appointment consumer ───────────────────────────────────────────────

// POSAppointmentCompletedHandler subscribes to pos.appointment.completed.
type POSAppointmentCompletedHandler struct {
	db     *ent.Client
	svc    *activities.Service
	logger *zap.Logger
}

func (h *POSAppointmentCompletedHandler) Subject() string { return "pos.appointment.completed" }
func (h *POSAppointmentCompletedHandler) Durable() string {
	return "marketflow-worker-crm-pos-appointment"
}

func (h *POSAppointmentCompletedHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID      string  `json:"tenant_id"`
		AppointmentID string  `json:"appointment_id"`
		ServiceName   string  `json:"service_name"`
		CRMContactID  *string `json:"crm_contact_id"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode pos.appointment.completed", err)
	}

	if ev.CRMContactID == nil || *ev.CRMContactID == "" {
		return nil
	}
	if h.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad tenant_id", err)
	}
	contactID, err := uuid.Parse(*ev.CRMContactID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad crm_contact_id", err)
	}

	subject := fmt.Sprintf("Appointment completed: %s", ev.ServiceName)
	now := time.Now()
	source := "pos-api"
	outcome := "completed"
	_, err = h.svc.Log(ctx, activities.CreateInput{
		TenantID:    tenantID,
		EntityType:  "contact",
		EntityID:    contactID,
		ActivityType: "meeting",
		Subject:     &subject,
		Outcome:     &outcome,
		Source:      &source,
		ExternalRef: &ev.AppointmentID,
		PerformedAt: &now,
	})
	if err != nil {
		h.logger.Error("failed to log appointment activity",
			zap.String("contact_id", contactID.String()), zap.Error(err))
		return err
	}
	return nil
}

// ── Treasury payment consumer ──────────────────────────────────────────────

// TreasuryPaymentSucceededHandler subscribes to treasury.payment.succeeded.
type TreasuryPaymentSucceededHandler struct {
	db     *ent.Client
	svc    *activities.Service
	logger *zap.Logger
}

func (h *TreasuryPaymentSucceededHandler) Subject() string { return "treasury.payment.succeeded" }
func (h *TreasuryPaymentSucceededHandler) Durable() string {
	return "marketflow-worker-crm-treasury-payment"
}

func (h *TreasuryPaymentSucceededHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev struct {
		TenantID     string  `json:"tenant_id"`
		IntentID     string  `json:"intent_id"`
		Reference    string  `json:"reference_id"`
		CRMContactID *string `json:"crm_contact_id"`
		Amount       float64 `json:"amount"`
		Currency     string  `json:"currency"`
		Method       string  `json:"payment_method"`
	}
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode treasury.payment.succeeded", err)
	}

	if ev.CRMContactID == nil || *ev.CRMContactID == "" {
		return nil
	}
	if h.db == nil {
		return nil
	}

	tenantID, err := uuid.Parse(ev.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad tenant_id", err)
	}
	contactID, err := uuid.Parse(*ev.CRMContactID)
	if err != nil {
		return dropPoison(h.logger, msg, "bad crm_contact_id", err)
	}

	subject := fmt.Sprintf("Payment %s %.2f via %s", ev.Currency, ev.Amount, ev.Method)
	body := fmt.Sprintf("Payment intent %s succeeded for reference %s", ev.IntentID, ev.Reference)
	now := time.Now()
	source := "treasury-api"
	outcome := "succeeded"
	_, err = h.svc.Log(ctx, activities.CreateInput{
		TenantID:    tenantID,
		EntityType:  "contact",
		EntityID:    contactID,
		ActivityType: "payment",
		Subject:     &subject,
		Body:        &body,
		Outcome:     &outcome,
		Source:      &source,
		ExternalRef: &ev.IntentID,
		PerformedAt: &now,
	})
	if err != nil {
		h.logger.Error("failed to log payment activity",
			zap.String("contact_id", contactID.String()), zap.Error(err))
		return err
	}
	return nil
}
