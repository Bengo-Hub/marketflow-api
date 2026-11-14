// Package autopublish carries an ad autopilot's video on by itself in the veto-window mode
// (Sprint 20, owner decision 2026-11-14). After the approve-each mode comes this one: the
// agent's draft goes script, storyboard, the storyboard approved by the agent, render, then an
// organic post scheduled VetoHours ahead. The owner gets a heads-up with a cancel link; a veto
// cancels the scheduled post before it runs. If nobody vetoes, the window ending is recorded as
// the approval. Paid boosts and ads never start here: they keep their explicit approval and a
// funded ad account.
//
// Every step goes through the Studio's own service calls, so the charges, refunds, the daily
// render cap and the agent's monthly credit ceiling all apply. A video that the guardrails flag
// (restricted categories, compliance findings, the tenant's hold categories), that would pass the
// ceiling, or that has nowhere to post falls back to approve-each: it waits for a person and the
// owner is emailed. State lives in the version's metadata.autopilot; each step the agent took is
// logged there, on the run's AgentRun trace and in AuditLog with the agent as the actor, and the
// person who vetoed (or "veto_window_elapsed") is recorded apart from it.
package autopublish

import (
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

// States of metadata.autopilot.state.
const (
	// StateActive: the agent is carrying the video on.
	StateActive = "active"
	// StateScheduled: the post is scheduled and the veto window is open.
	StateScheduled = "scheduled"
	// StateHeld: the video fell back to approve-each and waits for a person.
	StateHeld = "held"
	// StateVetoed: a person cancelled the post.
	StateVetoed = "vetoed"
	// StateReleased: the window ended without a veto; the post went ahead.
	StateReleased = "released"
	// StateFailed: a step failed for good; the draft is left for the owner.
	StateFailed = "failed"
	// StateHandedOver: a person approved, discarded or replaced the version meanwhile.
	StateHandedOver = "handed_over"
)

// Steps the pipeline takes.
const (
	stepWait              = "wait"
	stepHold              = "hold"
	stepStoryboard        = "storyboard"
	stepApproveStoryboard = "approve_storyboard"
	stepRender            = "render"
	stepSchedule          = "schedule"
	stepFailed            = "failed"
	stepHandOver          = "hand_over"
)

// Actors written to AuditLog and the step log.
const (
	ActorVetoWindow = "veto_window_elapsed"
	ActorVetoLink   = "veto_link"
)

func agentActor(agentID string) string { return "agent:" + agentID }

// maxSteps bounds the step log kept on a version.
const maxSteps = 30

// State is metadata.autopilot as the pipeline reads it.
type State struct {
	AgentID   string
	RunID     string
	State     string
	Reason    string
	VetoHours int
	PublishAt time.Time
	JobIDs    []string
	Raw       map[string]any
}

// readState reads a version's metadata.autopilot; nil when the version is not an autopilot one.
func readState(v *ent.CreativeVersion) *State {
	ap, ok := v.Metadata[creative.MetaAutopilot].(map[string]any)
	if !ok {
		return nil
	}
	st := &State{Raw: ap}
	st.AgentID, _ = ap["agent_id"].(string)
	st.RunID, _ = ap["run_id"].(string)
	st.State, _ = ap["state"].(string)
	st.Reason, _ = ap["reason"].(string)
	switch h := ap["veto_hours"].(type) {
	case float64:
		st.VetoHours = int(h)
	case int:
		st.VetoHours = h
	}
	if s, ok := ap["publish_at"].(string); ok {
		st.PublishAt, _ = time.Parse(time.RFC3339, s)
	}
	if ids, ok := ap["job_ids"].([]any); ok {
		for _, x := range ids {
			if s, ok := x.(string); ok {
				st.JobIDs = append(st.JobIDs, s)
			}
		}
	}
	if st.AgentID == "" {
		return nil
	}
	return st
}

// withStep returns the step log with one entry added, oldest dropped past maxSteps.
func withStep(raw map[string]any, entry map[string]any) []any {
	steps, _ := raw["steps"].([]any)
	out := append(append([]any{}, steps...), entry)
	if len(out) > maxSteps {
		out = out[len(out)-maxSteps:]
	}
	return out
}

// snapshot is what the next step depends on, read from the version, the agent and the tenant.
type snapshot struct {
	Status             string
	Presenter          bool // presenter kinds keep the storyboard; template ads skip it
	HasScript          bool
	Storyboard         bool
	StoryboardApproved bool
	StoryboardError    string
	Flags              []string // creative.FlaggedForReview
	HeldCategory       string   // the tenant's hold category the offering is in
	AgentOK            bool
	AgentWhy           string
}

// nextStep is the pipeline's one decision, kept pure so it is tested on its own.
func nextStep(s snapshot) (string, string) {
	switch s.Status {
	case creative.StatusApproved, creative.StatusRejected, creative.StatusSuperseded:
		return stepHandOver, "a person decided on this video"
	case creative.StatusFailed:
		return stepFailed, ""
	case creative.StatusQueued, creative.StatusRendering:
		return stepWait, ""
	case creative.StatusScriptReady, creative.StatusReady:
	default:
		return stepWait, ""
	}
	if !s.AgentOK {
		return stepHold, s.AgentWhy
	}
	if len(s.Flags) > 0 {
		return stepHold, strings.Join(s.Flags, "; ")
	}
	if s.HeldCategory != "" {
		return stepHold, fmt.Sprintf("you keep %q ads for your own approval", s.HeldCategory)
	}
	if s.Status == creative.StatusReady {
		return stepSchedule, ""
	}
	switch {
	case !s.HasScript:
		return stepHold, "the draft has no script"
	case !s.Presenter:
		// Owner 2026-11-14: a template ad goes from its script straight to the render.
		return stepRender, ""
	case s.StoryboardError != "":
		return stepHold, "the storyboard could not be made (" + s.StoryboardError + ")"
	case !s.Storyboard:
		return stepStoryboard, ""
	case !s.StoryboardApproved:
		return stepApproveStoryboard, ""
	}
	return stepRender, ""
}
