package snapshot

import (
	"strings"
	"testing"
)

// F-17. Quiesce resolved a disk to a filesystem with `findmnt --source /dev/sdc`,
// which asks "what is the WHOLE DEVICE mounted at". That only answers when the
// enrolled disk carries a filesystem directly on the bare device — the shape
// Linode's own attached volumes happen to have.
//
// A PARTITIONED data disk has nothing mounted at /dev/sdc, so the lookup failed
// and the cutover silently proceeded with a NON-quiesced copy of that disk,
// logged at info. Partitioned and LVM data disks are the norm coming from
// on-prem, AWS, Azure and GCP; the bare-device case is the special one.
func TestMountpointsFromLsblkFindsPartitions(t *testing.T) {
	// `lsblk -rno MOUNTPOINT /dev/sdc` on the E7 source: the whole device has no
	// mountpoint, each of its four partitions does.
	out := "\n/srv/e7-1\n/srv/e7-2\n/srv/e7-3\n/srv/e7-4\n"
	got := mountpointsFromLsblk(out)
	want := []string{"/srv/e7-4", "/srv/e7-3", "/srv/e7-2", "/srv/e7-1"} // deepest/last first
	if len(got) != len(want) {
		t.Fatalf("got %v, want %d mountpoints", got, len(want))
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("missing %q from %v", w, got)
		}
	}
}

// The bare-device case (a filesystem straight on /dev/sdc) must keep working —
// this is what every Linode-to-Linode migration looks like today.
func TestMountpointsFromLsblkBareDevice(t *testing.T) {
	got := mountpointsFromLsblk("/srv/appdata\n")
	if len(got) != 1 || got[0] != "/srv/appdata" {
		t.Errorf("got %v, want [/srv/appdata]", got)
	}
}

// An unmounted disk yields nothing — and that must be reported as "nothing to
// freeze", not silently treated as success.
func TestMountpointsFromLsblkUnmounted(t *testing.T) {
	for _, out := range []string{"", "\n", "\n\n\n"} {
		if got := mountpointsFromLsblk(out); len(got) != 0 {
			t.Errorf("lsblk %q: got %v, want none", out, got)
		}
	}
}

// Pseudo-mounts that lsblk can report must never be frozen: [SWAP] is not a
// filesystem, and freezing / on the SOURCE would wedge the whole box.
func TestMountpointsFromLsblkSkipsNonFilesystems(t *testing.T) {
	got := mountpointsFromLsblk("[SWAP]\n/srv/data\n")
	if contains(got, "[SWAP]") {
		t.Errorf("[SWAP] is not a filesystem and must not be frozen: %v", got)
	}
	if !contains(got, "/srv/data") {
		t.Errorf("real mountpoint dropped: %v", got)
	}
}

// Deepest-first ordering. Nested mounts must be frozen from the leaves inward,
// or freezing the parent blocks the walk to its children.
func TestMountpointsOrderedDeepestFirst(t *testing.T) {
	got := mountpointsFromLsblk("/srv\n/srv/data\n/srv/data/inner\n")
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if got[0] != "/srv/data/inner" || got[2] != "/srv" {
		t.Errorf("want deepest-first, got %v", got)
	}
}

// Duplicates (a device mounted twice, bind mounts) must be de-duplicated —
// freezing the same filesystem twice returns EBUSY on the second call.
func TestMountpointsDeduplicated(t *testing.T) {
	got := mountpointsFromLsblk("/srv/data\n/srv/data\n")
	if len(got) != 1 {
		t.Errorf("duplicate mountpoints must collapse to one, got %v", got)
	}
}

// The explicit override still wins: an operator who passes --mountpoint has told
// us exactly what to freeze.
func TestMountpointsForHonoursExplicitOverride(t *testing.T) {
	got, err := mountpointsFor(Options{Device: "/dev/sdc", Mountpoint: "/srv/chosen"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "/srv/chosen" {
		t.Errorf("got %v, want [/srv/chosen]", got)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// A disk with no mounted filesystem must produce a clear, quotable reason rather
// than a bare findmnt exit status — this string ends up in the console.
func TestNoMountpointsErrorIsExplicit(t *testing.T) {
	err := noMountpointsErr("/dev/sdc")
	for _, want := range []string{"/dev/sdc", "no mounted filesystem"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}
