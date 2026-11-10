package adsmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The boost row exists (as a draft the launch worker ignores) before any credits move, so a failed
// save never keeps credits; a refused charge removes the draft; only a paid boost goes live.
func TestCommitBoostOrder(t *testing.T) {
	type rec struct{ steps []string }
	run := func(createErr, chargeErr, activateErr error, paid bool) (rec, error) {
		var r rec
		err := commitBoost(context.Background(), boostSteps{
			create:   func() error { r.steps = append(r.steps, "create"); return createErr },
			charge:   func() (bool, error) { r.steps = append(r.steps, "charge"); return paid, chargeErr },
			activate: func() error { r.steps = append(r.steps, "activate"); return activateErr },
			discard:  func() { r.steps = append(r.steps, "discard") },
		})
		return r, err
	}

	r, err := run(nil, nil, nil, true)
	if err != nil || strings.Join(r.steps, ",") != "create,charge,activate" {
		t.Fatalf("happy path: %v %v", r.steps, err)
	}

	r, err = run(errors.New("db down"), nil, nil, true)
	if err == nil || strings.Join(r.steps, ",") != "create" {
		t.Fatalf("a failed save must not charge: %v %v", r.steps, err)
	}

	r, err = run(nil, nil, nil, false)
	if !errors.Is(err, errNoCredits) || strings.Join(r.steps, ",") != "create,charge,discard" {
		t.Fatalf("no credits: %v %v", r.steps, err)
	}

	r, err = run(nil, errors.New("billing down"), nil, false)
	if !errors.Is(err, errCreditsUnavailable) || strings.Join(r.steps, ",") != "create,charge,discard" {
		t.Fatalf("billing down: %v %v", r.steps, err)
	}

	r, err = run(nil, nil, errors.New("db down"), true)
	if err == nil || strings.Join(r.steps, ",") != "create,charge,activate" {
		t.Fatalf("a paid draft that cannot go live is kept, not discarded: %v %v", r.steps, err)
	}
}
