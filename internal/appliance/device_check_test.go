package appliance

import "testing"

const gib = int64(1 << 30)

// deviceSizeMismatch is the wrong-source-disk guard: when the agent's Hello
// declares a device size grossly different from the size the migration was
// created with, the agent is reading the WRONG DISK (migration #1 "game2": the
// migration declared 80 GiB but the agent's /dev/sda was a ~512 MiB swap disk,
// so the "full sync" was 496 MiB of swap and cutover failed at convert). The
// tolerance must stay generous — operators enter sizes in whole GB rounded up
// and disks are often a little smaller than nominal — so only gross mismatches
// (reported less than half or more than double the declared size) reject.
func TestDeviceSizeMismatch(t *testing.T) {
	cases := []struct {
		name               string
		declared, reported int64
		want               bool
	}{
		{"game2: 512MiB swap disk vs 80GiB declared", 80 * gib, 512 << 20, true},
		{"tiny data disk vs big declared", 100 * gib, 8 * gib, true},
		{"agent disk double the declared size", 40 * gib, 100 * gib, true},
		{"exact match", 80 * gib, 80 * gib, false},
		{"rounded up by operator (79.5GiB real)", 80 * gib, 80*gib - 512<<20, false},
		{"declared slightly small", 80 * gib, 85 * gib, false},
		{"half exactly is still accepted", 80 * gib, 40 * gib, false},
		{"declared unknown (0) — no check", 0, 512 << 20, false},
		{"reported unknown (0) — no check", 80 * gib, 0, false},
	}
	for _, c := range cases {
		if got := deviceSizeMismatch(c.declared, c.reported); got != c.want {
			t.Errorf("%s: deviceSizeMismatch(%d, %d) = %v, want %v", c.name, c.declared, c.reported, got, c.want)
		}
	}
}
