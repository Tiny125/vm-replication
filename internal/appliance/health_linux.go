//go:build linux

package appliance

import "syscall"

// applianceDiskFree reports free and total bytes of the filesystem holding path.
// One statfs syscall: the values come from the superblock already in memory, so
// it is safe to call on the console's existing 5-second settings poll even on a
// single vCPU busy with SHA-256 verification.
func applianceDiskFree(path string) (free, total int64, ok bool) {
	if path == "" {
		return 0, 0, false
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), true
}
