package adsmanager

import (
	"context"
	"errors"
	"fmt"
)

var (
	errNoCredits          = errors.New("not enough AI credits")
	errCreditsUnavailable = errors.New("AI credits could not be checked")
)

// boostSteps are the side effects of committing a boost, in the order commitBoost runs them.
type boostSteps struct {
	create   func() error         // save the boost as a draft the launch worker ignores
	charge   func() (bool, error) // charge the launch credits, idempotent on the boost id
	activate func() error         // mark it launching so the worker picks it up
	discard  func()               // remove the draft after a refused charge
}

// commitBoost saves the boost before any credits move, so a failed save can never keep credits,
// and a retry of the same boost id cannot charge twice (the charge key is the id). A refused
// charge removes the draft. A paid draft that cannot be marked launching is kept and reported,
// never discarded, so the charge stays traceable to its boost.
func commitBoost(_ context.Context, s boostSteps) error {
	if err := s.create(); err != nil {
		return fmt.Errorf("save boost: %w", err)
	}
	if s.charge != nil {
		ok, err := s.charge()
		if err != nil {
			s.discard()
			return fmt.Errorf("%w: %v", errCreditsUnavailable, err)
		}
		if !ok {
			s.discard()
			return errNoCredits
		}
	}
	if err := s.activate(); err != nil {
		return fmt.Errorf("start boost: %w", err)
	}
	return nil
}
