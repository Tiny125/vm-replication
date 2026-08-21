//go:build !linux

package appliance

// applianceDiskFree is Linux-only (statfs). Elsewhere it reports "unknown" so the
// console simply omits the line and `go test ./...` still builds for a
// contributor on another platform.
func applianceDiskFree(path string) (free, total int64, ok bool) { return 0, 0, false }
