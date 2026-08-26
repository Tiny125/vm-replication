package blockdiff

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the F-22 fix's wiring and error handling: that
// invalidation runs, and that a failure to invalidate is logged rather than
// propagated as a fatal error. Real page-cache staleness — a mounted
// filesystem writing a block while a raw device fd's cached page for that
// same block goes stale — cannot be observed from a regular file in a unit
// test; it requires root and a real block device with a live mount on top of
// it. That part of the fix is only provable on live hardware (see the task
// report).

// TestInvalidatePageCacheOnRegularFile exercises the real Linux/stub
// implementation against a plain regular file standing in for a device. On
// Linux, BLKFLSBUF is expected to fail (f is not a block device) while
// FADV_DONTNEED should succeed, so the combined call must still report
// success: only a total failure of both mechanisms is an error.
func TestInvalidatePageCacheOnRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test file: %v", err)
	}
	defer f.Close()

	if err := invalidatePageCache(f); err != nil {
		t.Errorf("invalidatePageCache(regular file) = %v, want nil", err)
	}
}

// TestInvalidatePageCacheClosedFile forces a real failure (bad fd) so we can
// confirm the helper surfaces it as an error instead of swallowing it or
// panicking; OpenDeviceRead is what's responsible for turning that error into
// a non-fatal warning (see TestOpenDeviceReadSurvivesInvalidationFailure).
func TestInvalidatePageCacheClosedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test file: %v", err)
	}
	f.Close() // fd is now invalid

	if err := invalidatePageCache(f); err == nil {
		t.Error("invalidatePageCache(closed fd) = nil, want an error")
	}
}

// TestOpenDeviceReadSurvivesInvalidationFailure wires a forced invalidation
// failure into OpenDeviceRead (via the package-level indirection point,
// invalidatePageCacheFn) and asserts the open still succeeds, with a clear
// warning logged that mentions staleness. This is the core F-22 requirement:
// a failure to invalidate the cache must never abort a migration, but it must
// not be silent either.
func TestOpenDeviceReadSurvivesInvalidationFailure(t *testing.T) {
	orig := invalidatePageCacheFn
	defer func() { invalidatePageCacheFn = orig }()
	invalidatePageCacheFn = func(f *os.File) error {
		return errors.New("forced failure for test")
	}

	var logBuf bytes.Buffer
	origOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origOut)

	path := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(path, []byte("hello, disk"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	dev, err := OpenDeviceRead(path)
	if err != nil {
		t.Fatalf("OpenDeviceRead must succeed even when cache invalidation fails: %v", err)
	}
	defer dev.Close()

	got := logBuf.String()
	if !strings.Contains(strings.ToLower(got), "stale") {
		t.Errorf("expected a warning mentioning staleness, got: %q", got)
	}
}

// TestOpenDeviceReadInvalidatesCacheByDefault is a smoke check that the real
// (non-injected) invalidation path runs without error for the common case —
// opening a regular file, as tests and file-image sources do.
func TestOpenDeviceReadInvalidatesCacheByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(path, []byte("hello, disk"), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	dev, err := OpenDeviceRead(path)
	if err != nil {
		t.Fatalf("OpenDeviceRead: %v", err)
	}
	defer dev.Close()
	if dev.Size != int64(len("hello, disk")) {
		t.Errorf("Size = %d, want %d", dev.Size, len("hello, disk"))
	}
}
