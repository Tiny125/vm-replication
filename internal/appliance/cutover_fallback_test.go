package appliance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// A guided BLOCK cutover whose source cannot be quiesced (mount -o remount,ro /
// fails — the NORMAL case on a running root, where systemd/journald always hold
// / open for writing) must NOT dead-end the cutover. It must fall back to the
// current crash-consistent replicated data — exactly what the "Skip the
// read-only snapshot" checkbox does — because the guided flow fsck-repairs the
// image during conversion and VALIDATES it is bootable before the operator
// powers anything off, so a genuinely bad image still fails fast safely.
//
// The old behavior aborted the cutover ("could NOT capture a consistent image
// … retry with Skip ticked"), which failed EVERY guided cutover of a normally
// running source and sent the operator hunting for a checkbox.
func TestGuidedCutoverFallsBackWhenQuiesceFails(t *testing.T) {
	oldPoll := consistencyPollEvery
	consistencyPollEvery = 5 * time.Millisecond
	defer func() { consistencyPollEvery = oldPoll }()

	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	s := &Server{
		st: st, ctx: ctx,
		receivers:      map[int64]*receiverHandle{},
		finalizes:      map[int64]context.CancelFunc{},
		consistReq:     map[int64]bool{},
		consistDone:    map[int64]bool{},
		quiesceErr:     map[int64]string{},
		pendingCutover: map[int64]api.FinalizeRequest{},
	}
	// No ConvertScript configured → the conversion step is skipped, so the test
	// exercises phase 1's control flow without shelling out.

	m, _, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "bb", SourceHostname: "bb", BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 25 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	// A recently-seen agent, so finalize actually asks for the quiesce.
	if err := st.RecordAgentConnected(ctx, m.Disks[0].ID); err != nil {
		t.Fatalf("record agent: %v", err)
	}
	m, err = st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload migration: %v", err)
	}

	done := make(chan struct{})
	go func() {
		s.finalize(ctx, m, api.FinalizeRequest{GuidedShutdown: true, LaunchInstance: true})
		close(done)
	}()
	// Once the quiesce request is up, report the agent's failure (busy root).
	deadline := time.Now().Add(5 * time.Second)
	for !s.wantDiskConsistency(m.Disks[0].ID) {
		if time.Now().After(deadline) {
			t.Fatal("finalize never requested a consistent pass")
		}
		time.Sleep(time.Millisecond)
	}
	s.markDiskQuiesceFailed(m.Disks[0].ID, "prepare source consistency: could not remount / read-only — a process is still writing to it")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("finalize did not finish")
	}

	got, err := st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	// The cutover must have CONTINUED to the awaiting-power-off pause, not
	// bounced back to replicating.
	if got.State != api.MigAwaitingCutover {
		t.Fatalf("state = %q, want %q (the cutover must proceed on the crash-consistent data, not abort)", got.State, api.MigAwaitingCutover)
	}
	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawFallback bool
	for _, e := range evs {
		if strings.Contains(e.Message, "continuing the cutover from the current crash-consistent replicated data") {
			sawFallback = true
		}
		if strings.Contains(e.Message, "could NOT capture a consistent image") {
			t.Errorf("the old hard-abort event must be gone, got: %s", e.Message)
		}
		if strings.Contains(e.Message, "then start the cutover again") {
			t.Errorf("no event may tell the operator to retry — the cutover continues automatically: %s", e.Message)
		}
	}
	if !sawFallback {
		t.Error("a clear fallback warning must be logged when the quiesce fails and the cutover continues")
	}
}
