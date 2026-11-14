package custom_agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/platform/legaldocs"
	"github.com/google/uuid"
)

// fakeTerms is auth-api's terms document and acceptances for the gate.
type fakeTerms struct {
	doc      legaldocs.Document
	docErr   error
	accepted []legaldocs.Acceptance
	accErr   error
}

func (f *fakeTerms) Document(context.Context) (legaldocs.Document, error) { return f.doc, f.docErr }
func (f *fakeTerms) Accepted(context.Context, uuid.UUID) ([]legaldocs.Acceptance, error) {
	return f.accepted, f.accErr
}
func (f *fakeTerms) Accept(_ context.Context, _ uuid.UUID, in legaldocs.AcceptInput) (legaldocs.Acceptance, error) {
	a := legaldocs.Acceptance{Version: in.Version, AcceptedAt: time.Now()}
	f.accepted = append(f.accepted, a)
	return a, nil
}

func TestTermsFromSource(t *testing.T) {
	now := time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)
	v1 := &legaldocs.Version{Version: "v1", EffectiveAt: now.Add(-24 * time.Hour)}
	v2 := &legaldocs.Version{Version: "v2", EffectiveAt: now.Add(10 * 24 * time.Hour)}
	acc := func(vs ...string) []legaldocs.Acceptance {
		out := []legaldocs.Acceptance{}
		for _, v := range vs {
			out = append(out, legaldocs.Acceptance{Version: v, AcceptedAt: now.Add(-time.Hour)})
		}
		return out
	}
	cases := []struct {
		name                      string
		src                       *fakeTerms
		legacy                    string
		wantAccepted, wantPending bool
		wantUnknown, wantDocument bool
		wantVersion               string
	}{
		{name: "auth-api down, nothing known", src: &fakeTerms{docErr: errors.New("down")}, legacy: "2026-11-14", wantUnknown: true, wantVersion: "cfg"},
		{name: "nothing published: config and metadata", src: &fakeTerms{doc: legaldocs.Document{}}, legacy: "cfg", wantAccepted: true, wantVersion: "cfg"},
		{name: "in force, accepted", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: v1}, accepted: acc("v1")}, wantAccepted: true, wantDocument: true, wantVersion: "v1"},
		{name: "in force, not accepted", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: v1}}, legacy: "cfg", wantDocument: true, wantVersion: "v1"},
		{name: "upcoming, accepted the one in force: notice", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: v1, Upcoming: v2}, accepted: acc("v1")}, wantAccepted: true, wantPending: true, wantDocument: true, wantVersion: "v2"},
		{name: "first version upcoming, metadata acceptance carries the notice", src: &fakeTerms{doc: legaldocs.Document{Published: true, Upcoming: v2}}, legacy: "cfg", wantAccepted: true, wantPending: true, wantDocument: true, wantVersion: "v2"},
		// Owner 2026-11-14: the in-app (metadata) acceptance never counts as accepting a published
		// version, even one with the same name; version "1" needs a fresh acceptance in auth-api.
		{name: "metadata acceptance does not accept a published version", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: v1}}, legacy: "v1", wantDocument: true, wantVersion: "v1"},
		{name: "metadata acceptance of the config version does not accept version 1", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: &legaldocs.Version{Version: "1", EffectiveAt: now.Add(-time.Hour)}}}, legacy: "2026-11-14", wantDocument: true, wantVersion: "1"},
		{name: "acceptances unreadable and not settled: unknown", src: &fakeTerms{doc: legaldocs.Document{Published: true, Current: v1}, accErr: errors.New("down")}, wantUnknown: true, wantDocument: true, wantVersion: "v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := AutoPublishGate{Terms: c.src, TermsVersion: "cfg", TermsPageURL: "https://accounts.example/legal/terms", Now: func() time.Time { return now }}
			st := AutoPublishStatus{TermsVersion: g.version()}
			g.termsFromSource(context.Background(), uuid.New(), &st, c.legacy, "2026-11-14T00:00:00Z")
			if st.TermsAccepted != c.wantAccepted || st.NewTermsPending != c.wantPending || st.TermsUnknown != c.wantUnknown || st.TermsDocument != c.wantDocument {
				t.Fatalf("got accepted=%v pending=%v unknown=%v document=%v", st.TermsAccepted, st.NewTermsPending, st.TermsUnknown, st.TermsDocument)
			}
			if st.TermsVersion != c.wantVersion {
				t.Fatalf("version = %q, want %q", st.TermsVersion, c.wantVersion)
			}
			if c.wantDocument && st.TermsURL != "https://accounts.example/legal/terms/"+c.wantVersion {
				t.Fatalf("terms_url = %q", st.TermsURL)
			}
		})
	}
}

// After the version's effective date an earlier acceptance no longer counts.
func TestTermsFromSource_NoticeEnds(t *testing.T) {
	v2 := &legaldocs.Version{Version: "v2", EffectiveAt: time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)}
	src := &fakeTerms{doc: legaldocs.Document{Published: true, Upcoming: v2}}
	g := AutoPublishGate{Terms: src, Now: func() time.Time { return v2.EffectiveAt.Add(time.Minute) }}
	st := AutoPublishStatus{}
	g.termsFromSource(context.Background(), uuid.New(), &st, "2026-11-14", "")
	if st.TermsAccepted || st.TermsUnknown {
		t.Fatalf("accepted=%v unknown=%v, want not accepted and known", st.TermsAccepted, st.TermsUnknown)
	}
}
