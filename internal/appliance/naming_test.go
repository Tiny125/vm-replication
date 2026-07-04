package appliance

import "testing"

// The operator can name the cutover instance (both boot methods) and the
// cutover volume (volume-boot) in the cutover dialog. Custom names are
// sanitized to Linode's label charset and capped (instances 64 chars, volumes
// 32); anything blank or unusable falls back to "<migration>-cutover".
func TestCutoverInstanceLabel(t *testing.T) {
	cases := []struct{ custom, mig, want string }{
		{"", "web01", "web01-cutover"},                                                        // blank -> default
		{"prod-app-01", "web01", "prod-app-01"},                                               // clean custom name
		{"My Prod Server!", "web01", "My-Prod-Server"},                                        // sanitized + trimmed
		{"x", "web01", "web01-cutover"},                                                       // too short after cleanup -> default
		{"--__--", "web01", "web01-cutover"},                                                  // nothing left -> default
		{string(make([]byte, 0)) + "srv" + repeat("a", 80), "web01", "srv" + repeat("a", 61)}, // capped at 64
	}
	for _, c := range cases {
		if got := cutoverInstanceLabel(c.custom, c.mig); got != c.want {
			t.Errorf("cutoverInstanceLabel(%q, %q) = %q, want %q", c.custom, c.mig, got, c.want)
		}
	}
}

func TestCutoverVolumeLabelFor(t *testing.T) {
	cases := []struct {
		custom, mig string
		idx, total  int
		want        string
	}{
		{"", "web01", 0, 1, "web01-cutover"},                     // blank -> existing default
		{"gamevol", "web01", 0, 1, "gamevol"},                    // clean custom name
		{"game vol!", "web01", 0, 1, "game-vol"},                 // sanitized + trimmed
		{"gamevol", "web01", 1, 3, "gamevol-1"},                  // multi-disk keeps per-disk suffix
		{repeat("v", 40), "web01", 0, 1, repeat("v", 32)},        // capped at 32
		{repeat("v", 40), "web01", 2, 3, repeat("v", 30) + "-2"}, // suffix survives the cap
	}
	for _, c := range cases {
		if got := cutoverVolumeLabelFor(c.custom, c.mig, c.idx, c.total); got != c.want {
			t.Errorf("cutoverVolumeLabelFor(%q, %q, %d, %d) = %q, want %q", c.custom, c.mig, c.idx, c.total, got, c.want)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
