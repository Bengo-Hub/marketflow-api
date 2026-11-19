package endpoints

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnknownEndpoint = errors.New("no such endpoint for this service")
	ErrBadStatus       = errors.New("status must be approved, rejected or candidate")
)

// Decide returns a copy of list with the owner's choice recorded on one route.
func Decide(list []Endpoint, method, path, status, by string, now time.Time) ([]Endpoint, error) {
	switch status {
	case StatusApproved, StatusRejected, StatusCandidate:
	default:
		return nil, ErrBadStatus
	}
	out := append([]Endpoint(nil), list...)
	for i := range out {
		if out[i].Method == method && out[i].Path == path {
			out[i].Status, out[i].DecidedBy, out[i].DecidedAt = status, by, now
			return out, nil
		}
	}
	return nil, ErrUnknownEndpoint
}

// Tool is one approved route as an agent sees it: the resource name it asks for, the URL with the
// tenant already in it, and the params it must supply.
type Tool struct {
	Service  string   `json:"service"`
	Resource string   `json:"resource"`
	URL      string   `json:"url"`
	Params   []string `json:"params,omitempty"`
	Summary  string   `json:"summary,omitempty"`
}

// ApprovedTools lists the approved routes of every known service for one tenant, sorted by
// service then resource. Services not scanned are skipped, and every route is checked against
// the filters again, so a route approved before a filter tightened is not served until it passes.
func ApprovedTools(all map[string][]Endpoint, services map[string]Service, tenantID uuid.UUID, slug string) []Tool {
	var out []Tool
	for svc, list := range all {
		s, ok := services[svc]
		if !ok {
			continue
		}
		base := strings.TrimSuffix(s.Base, "/")
		for _, e := range list {
			if e.Status != StatusApproved || e.Method != "GET" {
				continue
			}
			if ok, _ := Offerable(e.Method, e.Path, e.Tags, s.PublicPaths); !ok {
				continue
			}
			path, params := FillTenant(e.Path, tenantID, slug)
			out = append(out, Tool{Service: svc, Resource: ToolName(e.Path), URL: base + path, Params: params, Summary: e.Summary})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Resource < out[j].Resource
	})
	return out
}
