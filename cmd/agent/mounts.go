package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/tiny125/vm-replication/internal/protocol"
	"github.com/tiny125/vm-replication/internal/receiver"
)

// Mount enumeration and classification for the file-transfer walk.
//
// The walk used to stay on the root filesystem by comparing every entry's st_dev
// against the root's and skipping anything different. That was wrong twice over:
//
//   - It silently dropped real data. A database on a mounted volume was never
//     copied, and nothing said so.
//   - btrfs gives every SUBVOLUME its own anonymous st_dev, so on a default
//     SLES/openSUSE/Fedora source — btrfs root with subvolumes for /var, /home,
//     /srv, /opt, /usr/local, /root — the check treated most of the operating
//     system as a foreign filesystem and skipped it. Measured: a subvolume
//     reports dev=66 where its own filesystem root reports dev=65.
//
// The walk is now driven by MOUNTPOINTS read from /proc/self/mountinfo instead of
// device numbers. An unmounted btrfs subvolume is not a mountpoint, so the walk
// descends into it normally; a genuinely separate filesystem is a mountpoint, so
// it gets an explicit include/exclude decision that we can report.

// mountEntry is one line of /proc/self/mountinfo, reduced to what we need.
type mountEntry struct {
	Mountpoint string // absolute path on the source
	FSType     string
	Source     string // backing device or remote
	Options    string // the per-superblock + per-mount options we care to preserve
	DevID      string // "major:minor"
	Root       string // the subtree of the filesystem that is mounted (bind detection)
}

// copyableFSTypes are the local, file-backed filesystems worth copying. Anything
// absent is excluded by default: network filesystems (which can be unbounded and
// can hang the walk indefinitely), squashfs snap mounts, overlay container
// layers, EFI vfat, iso9660, fuse and the pseudo-filesystems.
var copyableFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "jfs": true, "f2fs": true,
	"reiserfs": true, "ufs": true,
}

// tmpfsReproducible are mountpoints that exist for isolation rather than
// capacity. They carry no data worth migrating and are reproduced on the
// destination as tmpfs, preserving their mount options at zero cost.
var tmpfsReproducible = map[string]bool{
	"/tmp": true, "/dev/shm": true, "/run": true, "/run/lock": true,
}

// parseMountinfo reads mount entries from an io source in /proc/self/mountinfo
// format. Split out from readMounts so it can be tested against fixtures rather
// than the host's live mount table.
func parseMountinfo(r *bufio.Scanner) []mountEntry {
	var out []mountEntry
	for r.Scan() {
		// 36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		//  0  1   2     3     4        5         6   7   8         9    10
		line := r.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 6 || len(right) < 2 {
			continue
		}
		out = append(out, mountEntry{
			DevID:      left[2],
			Root:       left[3],
			Mountpoint: unescapeMountField(left[4]),
			Options:    left[5],
			FSType:     right[0],
			Source:     unescapeMountField(right[1]),
		})
	}
	return out
}

// unescapeMountField decodes the octal escapes the kernel uses for spaces, tabs,
// newlines and backslashes in mountinfo paths.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			v := 0
			ok := true
			for _, c := range s[i+1 : i+4] {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// readMounts returns the source's current mount table.
func readMounts() ([]mountEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountinfo(bufio.NewScanner(f)), nil
}

// classifyMounts turns the mount table into the per-filesystem plan the walk
// follows and the appliance reports. root is the walk root (normally "/").
//
// excludeGlobs lets an operator drop a filesystem the heuristic would include —
// a NAS staging area, a scratch mount — without needing a code change.
func classifyMounts(mounts []mountEntry, root string, excludeGlobs []string) []protocol.FileRoot {
	root = filepath.Clean(root)
	seenBind := map[string]bool{} // devID+root, to drop bind mounts of something already covered
	var out []protocol.FileRoot

	for _, m := range mounts {
		mp := filepath.Clean(m.Mountpoint)
		// Only mounts at or beneath the walk root are relevant.
		if mp != root && !strings.HasPrefix(mp, strings.TrimSuffix(root, "/")+"/") {
			continue
		}
		rel := relOrEmpty(root, mp)

		fr := protocol.FileRoot{
			Path: rel, FSType: m.FSType, Source: m.Source,
			Options: mountOptionsOfInterest(m.Options),
		}
		switch {
		case mp == root:
			// The walk root itself is always included; it is the migration.
			fr.Included = true

		case m.FSType == "autofs":
			// Never stat through an automounter: doing so TRIGGERS the mount.
			fr.Reason = "autofs — walking it would trigger mounts"

		case tmpfsReproducible[mp] || m.FSType == "tmpfs" || m.FSType == "devtmpfs":
			fr.Reason = "in-memory filesystem — recreated on the destination, holds no data to migrate"
			fr.Reproduce = "tmpfs"

		case !copyableFSTypes[m.FSType]:
			fr.Reason = m.FSType + " — not a local data filesystem"

		case receiver.IsProtectedDestPath(strings.TrimPrefix(rel, "/")):
			fr.Reason = "excluded path — the destination keeps its own"

		case matchesAnyGlob(rel, excludeGlobs):
			fr.Reason = "excluded by the operator"

		case seenBind[m.DevID+" "+m.Root]:
			fr.Reason = "bind mount of a filesystem already being copied"

		default:
			fr.Included = true
			fr.Reproduce = "bind"
			seenBind[m.DevID+" "+m.Root] = true
		}
		out = append(out, fr)
	}
	return out
}

// relOrEmpty returns the root-relative form of an absolute mountpoint, "" for the
// root itself.
func relOrEmpty(root, mp string) string {
	if mp == root {
		return ""
	}
	r, err := filepath.Rel(root, mp)
	if err != nil {
		return mp
	}
	return r
}

// mountOptionsOfInterest keeps the options that are security or behaviour
// controls worth reproducing, and drops the noise. A separate filesystem is
// frequently a CONTROL (noexec on /tmp, nodev on /var) rather than a
// convenience, so dropping these silently would weaken the migrated host.
func mountOptionsOfInterest(opts string) string {
	keep := map[string]bool{"noexec": true, "nodev": true, "nosuid": true, "ro": true}
	var out []string
	for _, o := range strings.Split(opts, ",") {
		if keep[o] {
			out = append(out, o)
		}
	}
	return strings.Join(out, ",")
}

func matchesAnyGlob(rel string, globs []string) bool {
	for _, g := range globs {
		if g == "" {
			continue
		}
		if ok, _ := filepath.Match(strings.TrimPrefix(g, "/"), rel); ok {
			return true
		}
		if strings.HasPrefix(rel, strings.Trim(g, "/")+"/") {
			return true
		}
	}
	return false
}

// includedMountpoints returns the root-relative mountpoints the walk should
// descend into, and the ones it must skip. The walk skips AT a mountpoint rather
// than comparing device numbers, which is what makes btrfs subvolumes work.
func includedMountpoints(plan []protocol.FileRoot) (include, skip map[string]bool) {
	include, skip = map[string]bool{}, map[string]bool{}
	for _, fr := range plan {
		if fr.Path == "" {
			continue
		}
		if fr.Included {
			include[fr.Path] = true
		} else {
			skip[fr.Path] = true
		}
	}
	return include, skip
}
