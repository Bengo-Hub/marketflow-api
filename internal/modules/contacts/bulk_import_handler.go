package contacts

import (
	"net/http"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type bulkImportRowRequest struct {
	ExternalRef    string         `json:"external_ref"`
	ID             *uuid.UUID     `json:"id,omitempty"`
	Email          *string        `json:"email"`
	Phone          *string        `json:"phone"`
	FirstName      *string        `json:"first_name"`
	LastName       *string        `json:"last_name"`
	ContactType    string         `json:"contact_type"`
	LifecycleStage string         `json:"lifecycle_stage"`
	Metadata       map[string]any `json:"metadata"`
}

type bulkImportRequest struct {
	Contacts []bulkImportRowRequest `json:"contacts"`
}

// BulkImport handles POST /api/v1/contacts/bulk-import.
// Upserts a batch of contacts (dedup by phone then email, same rule as Create), so a migration
// can re-run the same payload safely. Each row may optionally carry a caller-supplied `id` for a
// stable crm_contact_id downstream services can reuse as a foreign key — the response always
// reports the ACTUAL id used per row (existing contacts win over a requested id).
//
//	@Summary      Bulk import/upsert contacts
//	@Tags         Contacts
//	@Accept       json
//	@Produce      json
//	@Param        X-Tenant-ID  header  string              true  "Tenant UUID"
//	@Param        request      body    bulkImportRequest   true  "Contacts to import"
//	@Success      200          {object}  BulkImportResult
//	@Security     bearerAuth
//	@Router       /contacts/bulk-import [post]
func (h *Handler) BulkImport(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}

	var req bulkImportRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if len(req.Contacts) == 0 {
		httpapi.Error(w, http.StatusBadRequest, "empty_batch", "contacts array is required and must be non-empty", nil)
		return
	}

	rows := make([]BulkImportRow, 0, len(req.Contacts))
	for _, c := range req.Contacts {
		rows = append(rows, BulkImportRow{
			ExternalRef:    c.ExternalRef,
			ID:             c.ID,
			Email:          c.Email,
			Phone:          c.Phone,
			FirstName:      c.FirstName,
			LastName:       c.LastName,
			ContactType:    c.ContactType,
			LifecycleStage: c.LifecycleStage,
			Metadata:       c.Metadata,
		})
	}

	result, err := h.service.BulkImport(r.Context(), tenantID, rows)
	if err != nil {
		h.logger.Error("bulk import failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "bulk_import_failed", "failed to bulk import contacts", nil)
		return
	}

	httpapi.JSON(w, http.StatusOK, result)
}
