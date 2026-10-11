package server

import "testing"

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"0.4.2", "0.4.1", true},
		{"v0.10.0", "0.9.0", true}, // numbers, not text
		{"1.0.0", "0.99.99", true},
		{"0.4.1", "0.4.1", false},
		{"0.4.0", "0.4.1", false},
		{"0.5.0-rc1", "0.4.1", false}, // pre-releases are not announced
		{"", "0.4.1", false},
		{"nonsense", "0.4.1", false},
	} {
		if got := newer(c.latest, c.current); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
