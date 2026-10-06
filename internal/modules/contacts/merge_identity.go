package contacts

import (
	"sort"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// metaMergedInto is the marker the old soft merge left on duplicates. Merge now deletes
// duplicates, so it is never carried onto a survivor.
const metaMergedInto = "merged_into"

// blank reports an unset optional string.
func blank(p *string) bool { return p == nil || strings.TrimSpace(*p) == "" }

// applyMergedIdentity fills the survivor's gaps from the duplicates (oldest first wins a gap) and
// adds up their order history. The survivor's own values are never overwritten, and metadata keys
// it lacks (the imported customer code legacy_contact_id and the like) are copied over.
func applyMergedIdentity(upd *ent.ContactUpdateOne, survivor *ent.Contact, dups []*ent.Contact) *ent.ContactUpdateOne {
	email, phone := survivor.Email, survivor.Phone
	first, last, flp := survivor.FirstName, survivor.LastName, survivor.FlpCustomerID
	lead, account, owner := survivor.LeadID, survivor.AccountID, survivor.OwnerUserID
	lastOrder := survivor.LastOrderAt
	totalCC, totalOrders := survivor.TotalCc, survivor.TotalOrders
	stage, kind := survivor.LifecycleStage, survivor.ContactType

	meta := make(map[string]any, len(survivor.Metadata))
	for k, v := range survivor.Metadata {
		meta[k] = v
	}
	delete(meta, metaMergedInto)

	for _, d := range oldestFirst(dups) {
		if blank(email) && !blank(d.Email) {
			email = d.Email
		}
		if blank(phone) && !blank(d.Phone) {
			phone = d.Phone
		}
		if blank(first) && !blank(d.FirstName) {
			first = d.FirstName
		}
		if blank(last) && !blank(d.LastName) {
			last = d.LastName
		}
		if blank(flp) && !blank(d.FlpCustomerID) {
			flp = d.FlpCustomerID
		}
		if lead == nil {
			lead = d.LeadID
		}
		if account == nil {
			account = d.AccountID
		}
		if owner == nil {
			owner = d.OwnerUserID
		}
		if d.LastOrderAt != nil && (lastOrder == nil || d.LastOrderAt.After(*lastOrder)) {
			lastOrder = d.LastOrderAt
		}
		totalCC += d.TotalCc
		totalOrders += d.TotalOrders
		// A duplicate that already bought makes the survivor a customer, never the reverse.
		if stage == "lead" && d.LifecycleStage != "" && d.LifecycleStage != "lead" {
			stage = d.LifecycleStage
		}
		if kind != "customer" && d.ContactType == "customer" {
			kind = d.ContactType
		}
		for k, v := range d.Metadata {
			if _, ok := meta[k]; !ok && k != metaMergedInto {
				meta[k] = v
			}
		}
	}

	upd.SetTotalCc(totalCC).SetTotalOrders(totalOrders).SetLifecycleStage(stage).SetContactType(kind).SetMetadata(meta)
	if !blank(email) {
		upd.SetEmail(*email).SetEmailNormalized(normalizeEmail(*email))
	}
	if !blank(phone) {
		upd.SetPhone(*phone)
		if digits := nationalSubscriberDigits(*phone); digits != "" {
			upd.SetPhoneSubscriberDigits(digits)
		}
	}
	upd.SetNillableFirstName(first).SetNillableLastName(last).SetNillableFlpCustomerID(flp).
		SetNillableLeadID(lead).SetNillableAccountID(account).SetNillableOwnerUserID(owner).
		SetNillableLastOrderAt(lastOrder)
	return upd
}

func oldestFirst(list []*ent.Contact) []*ent.Contact {
	out := append([]*ent.Contact(nil), list...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
