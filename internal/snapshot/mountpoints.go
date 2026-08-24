package snapshot

import (
	"fmt"
	"sort"
	"strings"
)

// mountpointsFromLsblk turns `lsblk -rno MOUNTPOINT <device>` output into the
// list of filesystems to quiesce, deepest path first.
//
// lsblk reports one line per block device in the tree — the disk itself and
// every partition or LV beneath it — with an empty line where nothing is
// mounted. That is exactly what is needed here: a partitioned disk has nothing
// mounted at the whole device but several mounted partitions, and the old
// whole-device lookup saw none of them.
//
// Deepest-first matters for nested mounts: freezing a parent before its child
// blocks the walk into the child.
func mountpointsFromLsblk(out string) []string {
	seen := map[string]bool{}
	var mps []string
	for _, line := range strings.Split(out, "\n") {
		mp := strings.TrimSpace(line)
		if mp == "" || seen[mp] {
			continue
		}
		// lsblk reports swap as the pseudo-target [SWAP]; it is not a filesystem
		// and cannot be frozen or remounted.
		if strings.HasPrefix(mp, "[") {
			continue
		}
		// Anything that is not an absolute path is not a mountpoint.
		if !strings.HasPrefix(mp, "/") {
			continue
		}
		seen[mp] = true
		mps = append(mps, mp)
	}
	sort.Slice(mps, func(i, j int) bool {
		di, dj := strings.Count(mps[i], "/"), strings.Count(mps[j], "/")
		if di != dj {
			return di > dj // deepest first
		}
		return mps[i] > mps[j]
	})
	return mps
}

// noMountpointsErr is the message an operator sees when a disk carries nothing
// mountable. It has to name the device and say what was actually wrong — the
// old failure surfaced as a bare `findmnt: exit status 1`.
func noMountpointsErr(device string) error {
	return fmt.Errorf("%s has no mounted filesystem to quiesce (nothing mounted from the disk or any of its partitions) — the copy will be crash-consistent rather than a point-in-time image; pass --mountpoint if the filesystem is mounted somewhere we cannot see", device)
}

// mountpointsFor returns every mounted filesystem living on o.Device — the disk
// itself and any partition or logical volume on it — or exactly o.Mountpoint
// when the operator named one.
func mountpointsFor(o Options) ([]string, error) {
	if o.Mountpoint != "" {
		return []string{o.Mountpoint}, nil
	}
	// -r raw, -n no header, -o MOUNTPOINT one column. Listing the DEVICE (not
	// -a) restricts the tree to this disk, so nothing on another disk is ever
	// frozen — the same "did it come with the migration" distinction the convert
	// script makes.
	out, err := run("lsblk", "-rno", "MOUNTPOINT", o.Device)
	if err != nil {
		// Fall back to the original whole-device lookup so a host without lsblk,
		// or a device lsblk refuses, behaves no worse than before.
		if fout, ferr := run("findmnt", "-n", "-o", "TARGET", "--source", o.Device); ferr == nil {
			if mps := mountpointsFromLsblk(fout); len(mps) > 0 {
				return mps, nil
			}
		}
		return nil, fmt.Errorf("could not list filesystems on %s: %w", o.Device, err)
	}
	mps := mountpointsFromLsblk(out)
	if len(mps) == 0 {
		return nil, noMountpointsErr(o.Device)
	}
	return mps, nil
}
