//go:build linux

package blockdiff

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// invalidatePageCache drops the kernel's cached pages for an already-open
// device fd so that reads which follow come from disk instead of a
// potentially-stale page-cache entry.
//
// Why this matters (F-22): the agent opens the source with a plain buffered
// os.OpenFile, so reads are served from the page cache keyed off the block
// device inode. That cache is independent of the page cache the mounted
// filesystem uses for the same underlying disk region, so it can hold a
// stale page for a region the mounted filesystem has since overwritten. The
// agent then hashes the stale bytes, records that hash as the checkpoint, and
// on every later pass reads the same stale cached page, gets the same hash,
// concludes "unchanged", and never resends — the corruption is silent and
// permanent. Dropping the cache before each pass's reads closes that gap.
//
// Two complementary mechanisms, both attempted:
//
//  1. BLKFLSBUF: the canonical "flush and invalidate this block device's
//     buffer cache" ioctl. This is the right tool when f really is a block
//     device, but requires CAP_SYS_ADMIN (the agent runs as root) and is a
//     no-op/error against a plain regular file (e.g. the --device flag
//     pointed at a file image rather than a real device).
//  2. FADV_DONTNEED: asks the kernel to drop cached pages for this fd's
//     entire range. It works for both regular files and block devices, so it
//     covers the case BLKFLSBUF can't reach.
//
// Only report an error when BOTH mechanisms failed to invalidate anything —
// that is the only case where we have no reason to believe the cache was
// actually dropped. The caller (OpenDeviceRead) treats even that as
// non-fatal: it logs a clear warning and proceeds, rather than aborting a
// migration over a best-effort cache hint.
func invalidatePageCache(f *os.File) error {
	fd := int(f.Fd())
	ioctlErr := unix.IoctlSetInt(fd, unix.BLKFLSBUF, 0)
	fadviseErr := unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)
	if ioctlErr != nil && fadviseErr != nil {
		return fmt.Errorf("BLKFLSBUF ioctl: %v; FADV_DONTNEED: %v", ioctlErr, fadviseErr)
	}
	return nil
}
