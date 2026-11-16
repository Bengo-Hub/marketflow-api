package marketingos

import "testing"

func TestBoostOTPThresholdKES(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want int64
	}{
		{"absent is the default", map[string]any{}, DefaultBoostOTPThresholdKES},
		{"nil metadata", nil, DefaultBoostOTPThresholdKES},
		{"zero turns it off", map[string]any{BoostOTPThresholdKey: float64(0)}, 0},
		{"a stored number", map[string]any{BoostOTPThresholdKey: float64(25000)}, 25000},
		{"negative falls back", map[string]any{BoostOTPThresholdKey: float64(-1)}, DefaultBoostOTPThresholdKES},
		{"a string falls back", map[string]any{BoostOTPThresholdKey: "5"}, DefaultBoostOTPThresholdKES},
	}
	for _, c := range cases {
		if got := BoostOTPThresholdKES(c.meta); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestCleanBoostOTPThreshold(t *testing.T) {
	v := func(n int64) *int64 { return &n }
	cases := []struct {
		in      *int64
		wantErr bool
	}{
		{nil, false},
		{v(0), false},
		{v(10000), false},
		{v(-5), true},
		{v(maxBoostOTPThresholdKES + 1), true},
	}
	for _, c := range cases {
		if err := cleanBoostOTPThreshold(c.in); (err != nil) != c.wantErr {
			t.Errorf("%v: err %v", c.in, err)
		}
	}
	var s Settings
	readBoostOTPThreshold(map[string]any{}, &s)
	if s.BoostOTPThresholdKES != nil {
		t.Fatal("absent key must read as null")
	}
	readBoostOTPThreshold(map[string]any{BoostOTPThresholdKey: float64(0)}, &s)
	if s.BoostOTPThresholdKES == nil || *s.BoostOTPThresholdKES != 0 {
		t.Fatal("zero must read back as zero")
	}
}
