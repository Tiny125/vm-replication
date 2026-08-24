package main

import (
	"fmt"
	"sort"
	"strings"
)

// skippedMounts records the paths a file-transfer pass did NOT copy because they
// live on a different filesystem from the root.
//
// Staying on the root filesystem is the correct scope — a separately mounted
// disk is its own disk, with its own size and its own migration. What was wrong
// was doing it silently: a source with data on a mounted volume migrated to a
// destination where that data was simply absent, while the console reported
// "Initial file copy complete" and then told the operator to power the source
// off. This type exists so the tool can say what it left behind.
type skippedMounts struct {
	paths []string
}

// add records a skipped path, ignoring anything already covered by a shallower
// entry (the walk reports the mountpoint first, then may report paths beneath
// it) and any duplicate.
func (s *skippedMounts) add(rel string) {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return
	}
	for _, p := range s.paths {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return // already covered by a recorded mountpoint
		}
	}
	s.paths = append(s.paths, rel)
}

// list returns the recorded mountpoints, sorted for a stable message.
func (s *skippedMounts) list() []string {
	if len(s.paths) == 0 {
		return nil
	}
	out := append([]string(nil), s.paths...)
	sort.Strings(out)
	return out
}

// warning is the operator-facing sentence, or "" when nothing was skipped.
//
// It names the paths, because "some filesystems were skipped" gives nobody
// anything to act on, and it is explicit that the data was NOT copied: this is
// the message standing between an operator and deleting the only copy of it.
func (s *skippedMounts) warning() string {
	l := s.list()
	if len(l) == 0 {
		return ""
	}
	const maxShown = 5
	shown := l
	suffix := ""
	if len(l) > maxShown {
		shown = l[:maxShown]
		suffix = fmt.Sprintf(" and %d more", len(l)-maxShown)
	}
	abs := make([]string, len(shown))
	for i, p := range shown {
		abs[i] = "/" + p
	}
	return fmt.Sprintf(
		"%d separate filesystem(s) were NOT copied by the file transfer: %s%s. File transfer only copies the root filesystem — add each of these disks to the migration (or use a block method) if their data must move.",
		len(l), strings.Join(abs, ", "), suffix)
}
