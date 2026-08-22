package main

import (
	"bufio"
	"strings"
	"testing"
)

// A realistic mountinfo from a SUSE-style source: btrfs root with SUBVOLUMES for
// /var, /home, /srv and /opt (each with its own anonymous device number), an
// attached data volume, plus the usual pseudo-filesystems, a snap squashfs, an
// EFI partition and an NFS mount.
const suseMountinfo = `
23 1 0:21 / /proc rw,relatime - proc proc rw
24 1 0:22 / /sys rw,relatime - sysfs sys rw
25 1 0:6 / /dev rw,relatime - devtmpfs dev rw
26 25 0:23 / /dev/shm rw,nosuid,nodev - tmpfs tmpfs rw
27 1 0:24 / /run rw,nosuid,nodev - tmpfs tmpfs rw
30 1 259:2 /@ / rw,relatime - btrfs /dev/sda2 rw,subvol=/@
31 30 259:2 /@/var /var rw,relatime,nodev - btrfs /dev/sda2 rw,subvol=/@/var
32 30 259:2 /@/home /home rw,relatime,nodev,nosuid - btrfs /dev/sda2 rw,subvol=/@/home
33 30 259:2 /@/srv /srv rw,relatime - btrfs /dev/sda2 rw,subvol=/@/srv
34 30 259:2 /@/opt /opt rw,relatime - btrfs /dev/sda2 rw,subvol=/@/opt
35 30 0:25 / /tmp rw,nosuid,nodev,noexec - tmpfs tmpfs rw
36 30 259:1 / /boot/efi rw,relatime - vfat /dev/sda1 rw
37 30 8:32 / /var/lib/pgsql rw,relatime - xfs /dev/sdc rw
38 30 0:44 / /snap/core/1234 ro,nodev - squashfs /dev/loop0 ro
39 30 0:55 / /mnt/nas rw,relatime - nfs4 10.0.0.5:/export rw
40 30 0:56 / /misc rw,relatime - autofs systemd-1 rw
`

func planFor(t *testing.T, mountinfo string, globs ...string) map[string]bool {
	t.Helper()
	ms := parseMountinfo(bufio.NewScanner(strings.NewReader(strings.TrimSpace(mountinfo))))
	inc := map[string]bool{}
	for _, fr := range classifyMounts(ms, "/", globs) {
		inc[fr.Path] = fr.Included
	}
	return inc
}

// The btrfs bug. SLES/openSUSE/Fedora default to a btrfs root whose /var, /home,
// /srv and /opt are SUBVOLUMES of the same filesystem. Because btrfs gives each
// subvolume its own anonymous st_dev (measured: root dev=65, subvolume dev=66),
// the old same-device walk treated them as foreign filesystems and skipped them —
// taking out essentially the whole operating system, while the source check
// recommended file transfer as "safer" for btrfs roots.
func TestBtrfsSubvolumesAreCopied(t *testing.T) {
	inc := planFor(t, suseMountinfo)
	for _, sub := range []string{"var", "home", "srv", "opt"} {
		if !inc[sub] {
			t.Errorf("btrfs subvolume /%s must be copied — skipping it guts a SUSE/Fedora source", sub)
		}
	}
}

// A genuinely separate data filesystem is the case that lost 5.1 GiB in live
// testing. It must be copied.
func TestSeparateDataFilesystemIsCopied(t *testing.T) {
	if !planFor(t, suseMountinfo)["var/lib/pgsql"] {
		t.Error("a mounted data filesystem must be copied; skipping it silently is the original defect")
	}
}

// Things that must NOT be copied, each for its own reason. Getting this wrong
// turns a bounded walk into an unbounded one — an NFS mount can be a petabyte and
// can hang the walk indefinitely, and a Docker host has hundreds of overlay mounts.
func TestNonDataFilesystemsAreExcluded(t *testing.T) {
	inc := planFor(t, suseMountinfo)
	for _, path := range []string{
		"proc", "sys", "dev", "dev/shm", "run", // pseudo
		"tmp",            // tmpfs — reproduced, not copied
		"boot/efi",       // vfat
		"snap/core/1234", // squashfs
		"mnt/nas",        // NFS: unbounded, can hang the walk
		"misc",           // autofs: walking it would TRIGGER mounts
	} {
		if inc[path] {
			t.Errorf("/%s must not be copied", path)
		}
	}
}

// The operator needs an escape hatch that does not require a code change: any
// classification heuristic will be wrong on somebody's machine.
func TestOperatorExcludeGlobs(t *testing.T) {
	if planFor(t, suseMountinfo, "var/lib/pgsql")["var/lib/pgsql"] {
		t.Error("an operator-excluded filesystem must not be copied")
	}
	// …and excluding one must not disturb the others.
	if !planFor(t, suseMountinfo, "var/lib/pgsql")["home"] {
		t.Error("excluding one filesystem must not affect the rest")
	}
}

// A bind mount of something already being copied would duplicate every byte.
func TestBindMountOfCopiedFilesystemIsSkipped(t *testing.T) {
	mi := suseMountinfo + "\n41 30 259:2 /@/srv /backup rw,relatime - btrfs /dev/sda2 rw,subvol=/@/srv"
	inc := planFor(t, mi)
	if !inc["srv"] {
		t.Error("the original mount should still be copied")
	}
	if inc["backup"] {
		t.Error("a bind mount of an already-copied filesystem must not be copied twice")
	}
}

// Mount options are frequently a security CONTROL, not a convenience. Dropping
// them silently would weaken the migrated host in a way an auditor notices and
// the operator does not.
func TestSecurityMountOptionsArePreserved(t *testing.T) {
	ms := parseMountinfo(bufio.NewScanner(strings.NewReader(strings.TrimSpace(suseMountinfo))))
	byPath := map[string]string{}
	for _, fr := range classifyMounts(ms, "/", nil) {
		byPath[fr.Path] = fr.Options
	}
	if !strings.Contains(byPath["home"], "nodev") || !strings.Contains(byPath["home"], "nosuid") {
		t.Errorf("/home's nodev,nosuid must be carried across; got %q", byPath["home"])
	}
	if !strings.Contains(byPath["var"], "nodev") {
		t.Errorf("/var's nodev must be carried across; got %q", byPath["var"])
	}
	if strings.Contains(byPath["home"], "relatime") {
		t.Errorf("only security-relevant options should be carried; got %q", byPath["home"])
	}
}

// Every excluded filesystem must carry a reason an operator can act on.
func TestExclusionsCarryAReason(t *testing.T) {
	ms := parseMountinfo(bufio.NewScanner(strings.NewReader(strings.TrimSpace(suseMountinfo))))
	for _, fr := range classifyMounts(ms, "/", nil) {
		if !fr.Included && fr.Reason == "" {
			t.Errorf("%q is excluded with no reason given", fr.Path)
		}
	}
}

// mountinfo escapes spaces and friends as octal; a path with a space must not
// silently become two fields.
func TestMountinfoUnescapesPaths(t *testing.T) {
	mi := "30 1 259:2 / / rw - ext4 /dev/sda1 rw\n31 30 8:32 / /srv/my\\040data rw - ext4 /dev/sdc rw"
	ms := parseMountinfo(bufio.NewScanner(strings.NewReader(mi)))
	found := false
	for _, m := range ms {
		if m.Mountpoint == "/srv/my data" {
			found = true
		}
	}
	if !found {
		t.Errorf("a mountpoint containing a space must be decoded; got %+v", ms)
	}
}
