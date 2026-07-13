package appliance

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// The destination watch must NOT give up: a receiver that comes up LATE (e.g.
// via the manual Lish install, after the automatic install stalled) must still
// be detected, flipping the destination to ready so Start replication unlocks.
// Before this fix the appliance polled for only 15 minutes and then stopped
// watching forever — the manual install command was useless after that.
func TestWatchFileDestDetectsLateReceiver(t *testing.T) {
	s := automationServer(t)
	const migID, instID = 7, 42

	// Pick a free port, but DON'T listen yet — the receiver is "not up".
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	_ = probe.Close()

	s.fileDests.Store(int64(migID), &fileDest{instanceID: instID, ip: "127.0.0.1", label: "late"})
	done := make(chan struct{})
	go func() {
		// Tiny intervals so the test runs fast; fastWindow 0 → straight to the
		// slow keep-watching loop (the state the 15-minute give-up used to end in).
		s.watchFileDest(migID, instID, addr, 0, 20*time.Millisecond, 20*time.Millisecond)
		close(done)
	}()

	// Let several polls fail first, then bring the receiver up LATE.
	time.Sleep(120 * time.Millisecond)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go acceptAndDiscard(ln)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := s.fileDests.Load(int64(migID)); ok && v.(*fileDest).ready {
			<-done // the watch loop must also exit once ready
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("late receiver was never detected — the watch gave up")
}

// The watch must stop when the destination record is dropped (migration
// deleted) instead of polling forever.
func TestWatchFileDestStopsWhenDropped(t *testing.T) {
	s := automationServer(t)
	const migID, instID = 8, 43
	s.fileDests.Store(int64(migID), &fileDest{instanceID: instID, ip: "127.0.0.1"})
	done := make(chan struct{})
	go func() {
		s.watchFileDest(migID, instID, "127.0.0.1:1", 0, 10*time.Millisecond, 10*time.Millisecond) // port 1: never up
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	s.fileDests.Delete(int64(migID))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not stop after the destination was dropped")
	}
}

// After an appliance restart the in-memory destination tracking is empty, but
// active file migrations persist their launched instance id — those must be
// selected for a resumed watch (block methods and finished migrations must not).
func TestFileDestsToResume(t *testing.T) {
	migs := []api.Migration{
		{ID: 1, BootTarget: api.BootTargetFile, State: api.MigReplicating, LaunchedID: 100},  // resume
		{ID: 2, BootTarget: api.BootTargetFile, State: api.MigAwaitingAgent, LaunchedID: 0},  // no dest yet
		{ID: 3, BootTarget: api.BootTargetVolume, State: api.MigReplicating, LaunchedID: 50}, // block: never
		{ID: 4, BootTarget: api.BootTargetFile, State: api.MigLaunched, LaunchedID: 101},     // finished
		{ID: 5, BootTarget: api.BootTargetFile, State: api.MigReady, LaunchedID: 102},        // resume
	}
	got := fileDestsToResume(migs)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 5 {
		ids := []int64{}
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		t.Fatalf("resume set = %v, want [1 5]", ids)
	}
}

// The manual install script must SHOW its output in the terminal (tee) — the
// operator pasting it in Lish saw nothing before, because everything was
// redirected to the log file only — and must say the console will detect it.
func TestDestInstallScriptIsVisible(t *testing.T) {
	s := &Server{}
	s.cfg.PublicHost = "203.0.113.10"
	s.cfg.ConsolePort = 8080
	ci := s.destCloudInit("tok")
	if !strings.Contains(ci, "tee -a /var/log/vmrepl-dest.log") {
		t.Error("install script must tee its output so a manual Lish run is visible")
	}
	if strings.Contains(ci, "exec >>/var/log") {
		t.Error("install script must not swallow output into the log file only")
	}
	if !strings.Contains(ci, "console will detect") {
		t.Error("install script should tell the operator the console detects the receiver by itself")
	}
}

// The destination panel presents the manual install as the "once the
// destination is up" path (per operator feedback), not a vague "taking too
// long" afterthought.
func TestConsoleDestPanelManualWording(t *testing.T) {
	if strings.Contains(consoleHTML, "Taking too long?") {
		t.Error("destPanel must not say 'Taking too long?'")
	}
	if !strings.Contains(consoleHTML, "Once the destination is up") {
		t.Error("destPanel should tell the operator to paste the install command once the destination is up")
	}
}
