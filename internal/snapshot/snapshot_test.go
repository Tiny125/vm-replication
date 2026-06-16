package snapshot

import "testing"

// TestDetectModeNonLVM is the safety guard for the cutover freeze regression:
// on a source with no LVM, auto-detection must return ModeNone (read live), not
// ModeFsfreeze. Holding an fsfreeze across a whole-device read blocks every
// write on the filesystem (root included) and wedges the source. lvs is absent
// (or errors on a bogus device) in CI, so isLVM is false and we expect ModeNone.
func TestDetectModeNonLVM(t *testing.T) {
	if got := DetectMode("/dev/does-not-exist-vmrepl-test"); got != ModeNone {
		t.Fatalf("DetectMode(non-LVM) = %q, want %q (must never auto-select a held freeze)", got, ModeNone)
	}
}

// TestPrepareNoneIsPassthrough confirms the default mode never touches the
// filesystem and reads the device directly.
func TestPrepareNoneIsPassthrough(t *testing.T) {
	readPath, cleanup, err := Prepare(Options{Mode: ModeNone, Device: "/dev/sdz"})
	if err != nil {
		t.Fatalf("Prepare(none): %v", err)
	}
	defer cleanup()
	if readPath != "/dev/sdz" {
		t.Fatalf("Prepare(none) readPath = %q, want /dev/sdz", readPath)
	}
}
