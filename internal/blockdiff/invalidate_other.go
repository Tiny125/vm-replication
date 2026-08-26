//go:build !linux

package blockdiff

import "os"

// invalidatePageCache is Linux-only (BLKFLSBUF / FADV_DONTNEED). Elsewhere it
// is a no-op so `go test ./...` still builds for a contributor on another
// platform; the agent only ships for Linux sources, so there is no
// stale-page-cache hazard to guard against off Linux in this codebase.
func invalidatePageCache(f *os.File) error { return nil }
