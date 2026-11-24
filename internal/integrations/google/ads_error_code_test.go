package google

import "testing"

// Google's 403 message is generic; the details carry the reason (2026-11-24: a new account read
// as "The caller does not have permission" until its setup was finished).
func TestAdsErrorCodeReadsTheDetails(t *testing.T) {
	body := []byte(`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED",
"details":[{"@type":"type.googleapis.com/google.ads.googleads.v21.errors.GoogleAdsFailure",
"errors":[{"errorCode":{"authorizationError":"CUSTOMER_NOT_ENABLED"},"message":"The customer account can't be accessed because it is not yet enabled or has been deactivated."}]}]}}`)
	if got := adsErrorCode(body); got != ErrCustomerNotEnabled {
		t.Fatalf("code %q", got)
	}
	if got := adsErrorCode([]byte(`{"error":{"message":"x"}}`)); got != "" {
		t.Fatalf("no details gave %q", got)
	}
	if got := adsErrorCode([]byte(`not json`)); got != "" {
		t.Fatalf("bad body gave %q", got)
	}
	e := &AdsError{Status: 403, Message: "The caller does not have permission", Code: ErrCustomerNotEnabled}
	if e.Error() != "google ads 403: The caller does not have permission (CUSTOMER_NOT_ENABLED)" {
		t.Fatalf("error text %q", e.Error())
	}
}
