package main

import (
	"strings"
	"testing"
)

// F-09 (CRITICAL, data loss). The file-transfer walk stays on the root
// filesystem by comparing st_dev, and SkipDir's anything on another device.
// That is the right SCOPE — a separately mounted disk is its own disk — but it
// was done silently: nothing recorded it, nothing warned, and the migration
// reported success. A source with 5.1 GiB on a mounted volume migrated to a
// destination with that data simply absent, while the console said
// "Initial file copy complete" and then "Power off the source now".
//
// Until the walk can copy other filesystems, it must at minimum SAY what it
// left behind.
func TestSkippedMountsRecordsWhatWasNotCopied(t *testing.T) {
	var s skippedMounts
	s.add("srv/appdata")
	s.add("srv/appdata/sub") // below an already-recorded mount: not a new one
	s.add("var/lib/mysql")
	s.add("srv/appdata") // duplicate

	got := s.list()
	if len(got) != 2 {
		t.Fatalf("want 2 distinct skipped mounts, got %v", got)
	}
	if !strings.Contains(strings.Join(got, ","), "srv/appdata") ||
		!strings.Contains(strings.Join(got, ","), "var/lib/mysql") {
		t.Errorf("both mountpoints must be recorded, got %v", got)
	}
}

// Nothing skipped must produce nothing — the happy path stays silent.
func TestSkippedMountsEmpty(t *testing.T) {
	var s skippedMounts
	if len(s.list()) != 0 {
		t.Error("no skips must yield no list")
	}
	if s.warning() != "" {
		t.Error("no skips must yield no warning")
	}
}

// The warning must name the paths and be unmistakable about the consequence:
// this is the message standing between an operator and deleting the only copy
// of their data.
func TestSkippedMountsWarningIsExplicit(t *testing.T) {
	var s skippedMounts
	s.add("srv/appdata")
	s.add("var/lib/mysql")
	w := s.warning()
	for _, want := range []string{"/srv/appdata", "/var/lib/mysql", "NOT copied"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning must contain %q, got: %s", want, w)
		}
	}
}

// A long list must stay readable in a console line rather than dumping
// hundreds of paths.
func TestSkippedMountsWarningBounded(t *testing.T) {
	var s skippedMounts
	for _, p := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		s.add("mnt/" + p)
	}
	w := s.warning()
	if len(w) > 400 {
		t.Errorf("warning should stay bounded, got %d chars: %s", len(w), w)
	}
	if !strings.Contains(w, "8") {
		t.Errorf("a truncated warning must still say how many there were: %s", w)
	}
}
