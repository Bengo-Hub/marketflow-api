package worker

import (
	"reflect"
	"testing"
)

func TestSweepSitesReadsSocialsAndSSOOnce(t *testing.T) {
	got := sweepSites("https://www.codevertexafrica.com/", "codevertexafrica.com", "")
	if !reflect.DeepEqual(got, []string{"https://www.codevertexafrica.com/"}) {
		t.Fatalf("same host twice: %v", got)
	}
	got = sweepSites("", "http://shop.example.co.ke")
	if !reflect.DeepEqual(got, []string{"https://shop.example.co.ke"}) {
		t.Fatalf("SSO only: %v", got)
	}
	got = sweepSites("https://a.example", "https://b.example")
	if len(got) != 2 {
		t.Fatalf("two sites: %v", got)
	}
}
