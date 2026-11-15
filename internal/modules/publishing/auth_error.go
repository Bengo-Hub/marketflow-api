package publishing

import "strings"

// reconnectMarkers are the parts of the platform errors the adapters record that mean the
// account's token no longer works. Only someone in the business connecting the account again
// fixes them; a retry never does. Matched in lower case against PublishJob.error.
var reconnectMarkers = []string{
	// Meta (Graph OAuthException, code 190 and its subcodes).
	"error validating access token",
	"session has expired",
	"session has been invalidated",
	"invalid oauth access token",
	"has not authorized application",
	"user has not authorized",
	"changed the password",
	"changed their password",
	// TikTok (Content Posting API error codes).
	"access_token_invalid",
	"scope_not_authorized",
	// Google (YouTube Data API and the OAuth token endpoint).
	"401 autherror",
	"invalid credentials",
	"invalid_grant",
	// MarketFlow's own wording when a stored grant was refused while renewing the token
	// (AccountTokenError).
	"no longer accepts this connection",
}

// NeedsReconnect says whether a recorded publishing error means the connected account must be
// connected again. The integrations page reads it to show a reconnect prompt on the card.
func NeedsReconnect(errText string) bool {
	if errText == "" {
		return false
	}
	e := strings.ToLower(errText)
	for _, m := range reconnectMarkers {
		if strings.Contains(e, m) {
			return true
		}
	}
	return false
}
