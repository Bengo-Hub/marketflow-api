package custom_agents

import "testing"

func TestClampMaxIterations(t *testing.T) {
	cases := []struct{ in, want int }{
		{-5, 1},
		{0, 1},
		{1, 1},
		{10, 10},
		{20, 20},
		{21, 20},
		{1_000_000, 20},
	}
	for _, c := range cases {
		if got := ClampMaxIterations(c.in); got != c.want {
			t.Errorf("ClampMaxIterations(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
