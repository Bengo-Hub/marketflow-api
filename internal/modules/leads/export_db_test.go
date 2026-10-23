package leads

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// ExportEach must return every matching lead exactly once, newest first, across
// page boundaries and timestamp ties.
func TestExportEachKeysetPagesAcrossTies(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)

	prev := exportPage
	exportPage = 2
	t.Cleanup(func() { exportPage = prev })

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	want := map[uuid.UUID]bool{}
	// Three leads share one timestamp, so the id tie-break decides page edges.
	for i, at := range []time.Time{base, base, base, base.Add(time.Hour), base.Add(-time.Hour)} {
		l, err := client.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").
			SetStatus("new").SetFirstName("L").SetCreatedAt(at).Save(ctx)
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		want[l.ID] = true
	}
	// One lead from another source must be filtered out.
	other := "facebook"
	if _, err := client.Lead.Create().SetTenantID(tid).SetSource(other).SetLeadType("customer").
		SetStatus("new").SetFirstName("X").SetCreatedAt(base).Save(ctx); err != nil {
		t.Fatal(err)
	}

	src := "website"
	var got []*ent.Lead
	if err := svc.ExportEach(ctx, ListInput{TenantID: tid, Source: &src}, func(l *ent.Lead) error {
		got = append(got, l)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("exported %d leads, want %d", len(got), len(want))
	}
	seen := map[uuid.UUID]bool{}
	for i, l := range got {
		if !want[l.ID] || seen[l.ID] {
			t.Fatalf("lead %s unexpected or repeated", l.ID)
		}
		seen[l.ID] = true
		if i > 0 && l.CreatedAt.After(got[i-1].CreatedAt) {
			t.Fatal("export is not newest first")
		}
	}
}
