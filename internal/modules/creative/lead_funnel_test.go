package creative

import (
	"context"
	"errors"
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestEnsureLeadFunnelIsSafeToRepeat(t *testing.T) {
	calls := 0
	s := NewService(nil, zap.NewNop()).WithLeadFunnels(func(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string) (uuid.UUID, string, error) {
		calls++
		return uuid.New(), "x", nil
	})
	b := &ent.CreativeBrief{ID: uuid.New(), Metadata: map[string]any{"funnel_id": uuid.NewString()}}
	got, err := s.ensureLeadFunnel(context.Background(), b)
	if err != nil || got != b || calls != 0 {
		t.Fatalf("an ad with a page got another: err %v, calls %d", err, calls)
	}
}

func TestEnsureLeadFunnelWithoutWiring(t *testing.T) {
	s := NewService(nil, zap.NewNop())
	if _, err := s.ensureLeadFunnel(context.Background(), &ent.CreativeBrief{ID: uuid.New()}); !errors.Is(err, ErrNoLeadFunnels) {
		t.Fatalf("err = %v", err)
	}
	// The create path never fails the ad for it.
	b := &ent.CreativeBrief{ID: uuid.New()}
	if got := s.attachLeadFunnel(context.Background(), b); got != b {
		t.Fatal("attach changed the ad without lead pages wired")
	}
}
