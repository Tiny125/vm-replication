package appliance

import (
	"testing"
	"time"
)

// diskCopyTimeout budgets the wait for the disk-mode cutover's in-guest copy
// (volume → local disk, ending in a self-power-off). The old fixed 60 minutes
// failed real migrations: gamev2's ~80 GiB copy was still running when the
// appliance gave up. The budget scales with the disk size at a conservative
// 10 MiB/s plus boot slack, floored at 1h (small disks) and capped at 8h
// (runaway guard).
func TestDiskCopyTimeout(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		// 80 GiB (gamev2): 81920 MiB / 10 MiB/s = 8192s, + 15m slack.
		{"80GiB scales past the old 60m", 80 * gib, 15*time.Minute + 8192*time.Second},
		// Small disks hit the 1h floor.
		{"10GiB floors at 1h", 10 * gib, time.Hour},
		{"zero size floors at 1h", 0, time.Hour},
		// Enormous disks cap at 8h.
		{"1TiB caps at 8h", 1 << 40, 8 * time.Hour},
	}
	for _, c := range cases {
		if got := diskCopyTimeout(c.size); got != c.want {
			t.Errorf("%s: diskCopyTimeout(%d) = %s, want %s", c.name, c.size, got, c.want)
		}
	}
	if got := diskCopyTimeout(80 * gib); got <= time.Hour {
		t.Errorf("an 80 GiB copy budget (%s) must exceed the old fixed 60m", got)
	}
}
