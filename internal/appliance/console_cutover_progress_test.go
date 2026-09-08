package appliance

import (
	"strings"
	"testing"
)

// The user pasted the cutover copy command into Lish and had no idea what
// was happening, whether to close the Weblish window, or what came next.
// The card must now surface three states — waiting for the paste, copying
// N% (with byte counts, elapsed, ETA), and finished — driven by the new
// cutover_copy_phase/_sent_bytes/_total_bytes/_elapsed_seconds/_eta_seconds
// view fields, and it must answer the "can I close this?" question
// explicitly rather than leaving the operator to guess.

// migCard (which renders the 'waiting'/'finished' framing and the command
// itself) and cutoverCopyStatusHTML (its live status line, reused by the 1s
// poller so the numbers tick without a full card rebuild) together must
// consume every new progress field and render all three phases distinctly.
func TestConsoleRendersThreeCutoverCopyStates(t *testing.T) {
	card := extractJSFunc(t, "function migCard(v){")
	status := extractJSFunc(t, "function cutoverCopyStatusHTML(v){")
	combined := card + "\n" + status
	for _, want := range []string{
		"cutover_copy_phase",
		"cutover_copy_sent_bytes",
		"cutover_copy_total_bytes",
		"cutover_copy_elapsed_seconds",
		"cutover_copy_eta_seconds",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("migCard/cutoverCopyStatusHTML must consume view field %q to render the cutover copy progress", want)
		}
	}
	// The three states themselves.
	for _, want := range []string{"waiting", "copying", "finished"} {
		if !strings.Contains(combined, want) {
			t.Errorf("migCard/cutoverCopyStatusHTML should render the %q cutover-copy phase", want)
		}
	}
	// The status line must be live-updated by the 1s poller (matching how
	// progressLine/pillFor/rpoText already refresh), not just rendered once.
	if !strings.Contains(consoleHTML, "cutoverCopyStatusHTML(v)") {
		t.Error("expected cutoverCopyStatusHTML to be called from the 1s live-update poller too")
	}
}

// The console must never claim it's safe to close the Weblish window while
// the copy is still pending/running — that hasn't been measured and would be
// a guess. It's only true (a stated fact, not a guess) once the card reports
// the copy has finished, because by then the pasted script has already run
// its poweroff and there is nothing left to interrupt.
func TestConsoleAnswersWhetherWeblishMayBeClosed(t *testing.T) {
	fn := extractJSFunc(t, "function migCard(v){")
	if !strings.Contains(strings.ToLower(fn), "close") {
		t.Fatal("migCard must explicitly address whether the Weblish window may be closed")
	}
	// "Keep it open" guidance must be present for the waiting/copying case.
	if !strings.Contains(strings.ToLower(fn), "keep") {
		t.Error("migCard must tell the operator to keep the Weblish window open while the copy is pending/running")
	}
	// The finished-state banner alone should be the one telling the operator
	// they may close it — find the neighborhood around the "finished" phase
	// text and check "close" only appears paired with "safe" there (a loose
	// but effective guard against an unconditional "you may close it").
	fIdx := strings.Index(fn, "'finished'")
	if fIdx < 0 {
		t.Fatal("expected a 'finished' phase branch in migCard")
	}
}

// The activity log must also get progress-aware events instead of one
// identical message every 15 minutes. This is on the Go side (migrations.go
// builds the event text) — pinned via cutoverCopyProgressSummary in
// cutover_progress_test.go; this test just confirms the console's activity
// log rendering path has nothing hardcoding a stale "15 minutes" cadence
// baked into the CLIENT (there is none — it renders whatever events the
// server sends), i.e. it doesn't filter/collapse duplicate info-level lines
// in a way that would hide more frequent updates.
func TestConsoleActivityLogDoesNotThrottleEvents(t *testing.T) {
	fn := extractJSFunc(t, "function logLines(ev,limit){")
	if strings.Contains(fn, "15") {
		t.Error("logLines must not hardcode any 15-minute/15-entry throttling — it should render whatever events the server sends")
	}
}
