package analyze

import "testing"

func TestCallTakesRangeVector(t *testing.T) {
	tests := []struct {
		args string
		want bool
	}{
		{`(x[5m])`, true},
		{`(x{job="a"}[5m] offset 1h)`, true},
		{`(rate(x[5m])[30m:1m])`, true},
		{`(x)`, false},
		{`(x{path=~"[a-z]+"})`, false},
		{`(rate(x[5m]))`, false},
		{`(x) + y[5m]`, false},
	}
	for _, tc := range tests {
		if got := callTakesRangeVector(tc.args, 0); got != tc.want {
			t.Errorf("callTakesRangeVector(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
