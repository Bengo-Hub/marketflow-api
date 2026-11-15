package publishing

import "testing"

func TestNeedsReconnect(t *testing.T) {
	yes := []string{
		"facebook start: 400 OAuthException: Error validating access token: Session has expired on Friday, 13-Nov-26 10:00:00 PST.",
		"gave up after 5 tries: instagram container: 400 OAuthException: Error validating access token: The user has not authorized application 123.",
		"tiktok creator info: access_token_invalid: The access token is invalid or not found in the request.",
		"youtube start: 401 authError: Invalid Credentials",
	}
	for _, e := range yes {
		if !NeedsReconnect(e) {
			t.Errorf("NeedsReconnect(%q) = false, want true", e)
		}
	}
	no := []string{
		"",
		"this Instagram account has used its posts for the last 24 hours; try again later",
		"facebook upload: status 503",
		"the platform was still processing the video after 2 hours; check the account and post again",
		"publishing to this platform is not switched on yet",
	}
	for _, e := range no {
		if NeedsReconnect(e) {
			t.Errorf("NeedsReconnect(%q) = true, want false", e)
		}
	}
}
