package testimonials

import "testing"

func ptr[T any](v T) *T { return &v }

func TestValidate(t *testing.T) {
	long := make([]rune, 121)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name string
		req  upsertRequest
		code string
	}{
		{"empty update is fine", upsertRequest{}, ""},
		{"publish toggle alone is fine", upsertRequest{IsPublished: ptr(false)}, ""},
		{"name too long", upsertRequest{AuthorName: ptr(string(long))}, "invalid_author_name"},
		{"rating out of range", upsertRequest{Rating: ptr(6)}, "invalid_rating"},
		{"rating zero", upsertRequest{Rating: ptr(0)}, "invalid_rating"},
		{"javascript avatar", upsertRequest{AvatarURL: ptr("javascript:alert(1)")}, "invalid_avatar_url"},
		{"relative avatar", upsertRequest{AvatarURL: ptr("/me.png")}, "invalid_avatar_url"},
		{"https avatar", upsertRequest{AvatarURL: ptr(" https://cdn.example.com/a.png ")}, ""},
		{"empty avatar clears", upsertRequest{AvatarURL: ptr("")}, ""},
	}
	for _, c := range cases {
		req := c.req
		if code, _ := req.validate(); code != c.code {
			t.Errorf("%s: code %q, want %q", c.name, code, c.code)
		}
	}
}

func TestValidateTrims(t *testing.T) {
	req := upsertRequest{AuthorName: ptr("  Wanjiru  "), AvatarURL: ptr(" https://x.co/a.png ")}
	req.validate()
	if *req.AuthorName != "Wanjiru" || *req.AvatarURL != "https://x.co/a.png" {
		t.Fatalf("not trimmed: %q %q", *req.AuthorName, *req.AvatarURL)
	}
}
