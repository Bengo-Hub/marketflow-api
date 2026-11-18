package creative

import (
	"net/http"
	"strings"

	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Who may set what in a brief's metadata (2026-11-18). The services write system keys that other
// code trusts: source, channel, run_id and requested_by (a Marketing Manager turn, which routes
// the storyboard and the finished video to that person's WhatsApp), autopilot_agent_id and
// autopilot_run_id (the agent's reports, ceiling and dashboard), brand, funnel_slug and the
// rest. A person may set only personBriefMetaKeys; everything else they send is dropped, and an
// update merges their keys into the brief's metadata instead of replacing it, so a person can
// neither forge a system key nor wipe one.
//
// The system keys come from two places, and both keep working: the autopilot worker calls the
// Service directly (no HTTP, no filter), and marketflow-ai's make-media calls
// POST /creative/briefs with the service key and no acting person. A service key that carries
// X-Acting-User-ID acts for a person (the AI agent's tools, guardrail.ActingTransport), so it gets
// the person's allowlist too.

// personBriefMetaKeys are the metadata keys a signed-in person may set: the Studio form's
// Creative Plan (brief_plan.go). Grep marketflow-ui for BriefInput.metadata when adding one.
var personBriefMetaKeys = map[string]bool{"plan": true}

// systemBriefCaller reports whether the request may set system metadata keys: a service key that
// acts for no person.
func systemBriefCaller(r *http.Request) bool {
	c, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || c == nil || !c.IsService {
		return false
	}
	return strings.TrimSpace(r.Header.Get(httpapi.HeaderActingUserID)) == ""
}

// personBriefMetadata keeps only the keys a person may set; nil when none is left.
func personBriefMetadata(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range meta {
		if personBriefMetaKeys[k] {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// guardBriefMetadata applies the rule to a create or update body: a person's metadata is cut to
// the allowlist, and on an update merged into the brief's own (BriefInput.mergeMetadata).
func guardBriefMetadata(r *http.Request, in *BriefInput, update bool) {
	if systemBriefCaller(r) {
		return
	}
	in.Metadata = personBriefMetadata(in.Metadata)
	in.mergeMetadata = update
}
