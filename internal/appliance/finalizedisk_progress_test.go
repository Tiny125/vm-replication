package appliance

import (
	"os"
	"strings"
	"testing"
)

// A real user pasted the cutover copy command into Lish and had no idea what
// was happening for the whole 30-90+ minute copy: the wait loop emitted one
// IDENTICAL message every 15 minutes (same whether nothing had been pasted
// or the copy was 90% done), and a second silent gap followed between
// "image copied" and the final completion event (volume hydration, config
// creation, attach, boot — nothing logged in between).
//
// finalizeDisk (migrations.go) is the orchestration function that runs all
// of this; it isn't practically unit-testable end to end (it needs a live/
// fake Linode client for a dozen async steps), so — matching the existing
// pattern in this package for exactly this shape (see
// TestCutoverImageStreamInvalidatesPageCacheFirst in cutover_stream_test.go,
// which reads the handler's own source and asserts on it) — these tests read
// finalizeDisk's own source and pin the specific fixes: progress-aware
// events replacing the fixed 15-minute cadence, the timeout failure message
// reporting how far the copy got, the true "finished" signal being set from
// the instance's confirmed power-off, and one event per remaining step.
func extractFinalizeDisk(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (s *Server) finalizeDisk(")
	if i < 0 {
		t.Fatal("finalizeDisk not found in migrations.go")
	}
	body := s[i:]
	// Cut at the next top-level (column-0) func declaration.
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	return body
}

// The copy-wait loop must use cutoverCopyProgressSummary (built from the
// real, incrementally-tracked byte count) for its periodic events instead of
// a fixed message, and must not emit on a flat 15-minute cadence regardless
// of progress.
func TestFinalizeDiskEmitsProgressAwareEvents(t *testing.T) {
	body := extractFinalizeDisk(t)
	if !strings.Contains(body, "cutoverCopyProgressSummary(") {
		t.Error("finalizeDisk's copy-wait loop must build its progress events from cutoverCopyProgressSummary, not a fixed string")
	}
	if !strings.Contains(body, "cutoverCopyViewFor(m.ID)") {
		t.Error("finalizeDisk must read the real tracked progress (cutoverCopyViewFor) while waiting for the copy")
	}
	// The old bug: an identical message regardless of how far the copy had
	// gotten. Guard against reintroducing a bare "waiting for the rescue
	// copy to finish" with no percentage/byte information alongside it.
	if strings.Contains(body, `"cutover: waiting for the rescue copy to finish (%s elapsed of up to %s) — if you haven't yet, paste the copy command from this card into the instance's Lish console"`) {
		t.Error("the old fixed-cadence, progress-blind wait message is still present — it must be replaced with a progress-aware one")
	}
}

// The copy-budget-timeout failure must report how far the copy actually got.
func TestFinalizeDiskTimeoutReportsProgress(t *testing.T) {
	body := extractFinalizeDisk(t)
	// Find the s.fail(...) call for the copy-budget-exhausted path (the one
	// mentioning "did not power the instance off").
	i := strings.Index(body, "did not power the instance off within")
	if i < 0 {
		t.Fatal("expected the copy-budget-timeout failure message")
	}
	// cutoverCopyProgressSummary must be used to build (or be included in)
	// that failure text, not just logged separately.
	surrounding := body[max0(i-800):i]
	if !strings.Contains(surrounding+body[i:i+800], "cutoverCopyProgressSummary(") {
		t.Error("the copy-budget-timeout failure must report progress via cutoverCopyProgressSummary, not a generic 'it didn't finish'")
	}
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

// The TRUE "copy finished" signal is the instance's confirmed power-off, not
// merely all bytes having been handed to the network — cutoverCopyMarkFinished
// must be called from the success path of the wait loop (right alongside the
// existing dropCutoverStream call, which erases the stream the instant this
// runs — that's exactly why the copy-progress state needed its own map).
func TestFinalizeDiskMarksCopyFinishedOnPowerOff(t *testing.T) {
	body := extractFinalizeDisk(t)
	if !strings.Contains(body, "cutoverCopyMarkFinished(m.ID)") {
		t.Error("finalizeDisk must call cutoverCopyMarkFinished(m.ID) once the instance is confirmed powered off")
	}
	fIdx := strings.Index(body, "cutoverCopyMarkFinished(m.ID)")
	dIdx := strings.Index(body, "s.dropCutoverStream(m.ID) // copy done")
	if fIdx < 0 || dIdx < 0 {
		t.Fatal("expected both cutoverCopyMarkFinished and the explicit post-copy dropCutoverStream call")
	}
	if !(fIdx < dIdx) {
		t.Error("cutoverCopyMarkFinished must be called BEFORE dropCutoverStream erases the stream, or the console could momentarily see neither a command nor a finished phase")
	}
}

// The second silent gap: nothing was logged between "image copied" and the
// final completion event, even though the appliance runs several more
// distinct steps (hydrate data volumes, create the boot config, wait for
// volumes to attach, request the boot). Each must now get its own event.
func TestFinalizeDiskLogsEveryPostCopyStep(t *testing.T) {
	body := extractFinalizeDisk(t)
	copiedIdx := strings.Index(body, "image copied onto the local disk")
	launchIdx := strings.Index(body, "diskBootLaunchEvent(")
	if copiedIdx < 0 || launchIdx < 0 {
		t.Fatal("expected both the 'image copied' event and the final diskBootLaunchEvent call")
	}
	gap := body[copiedIdx:launchIdx]
	addEvents := strings.Count(gap, "AddEvent(")
	if addEvents < 3 {
		t.Errorf("expected at least 3 AddEvent calls between 'image copied' and the final launch event (hydration/config/attach/boot are otherwise silent); found %d in:\n%s", addEvents, gap)
	}
}
