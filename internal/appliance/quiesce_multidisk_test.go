package appliance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// F-14: quiesceForCutover used to return false the moment ANY disk reported
// it could not quiesce, abandoning the wait for every OTHER disk even though
// they would very likely still land a clean crash-consistent pass. Measured
// live on a two-disk source (findings.md F-14): the two disks land their
// final passes ~54s apart, so a fail-fast on the (nearly-always-refusing)
// root threw away a data volume that would otherwise have quiesced cleanly.
//
// These tests build a Server against a real (in-memory) store, following the
// pattern in cutover_fallback_test.go, and drive quiesceForCutover's disk
// state directly instead of running real agents.

func newQuiesceTestServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	oldPoll := consistencyPollEvery
	consistencyPollEvery = 5 * time.Millisecond
	t.Cleanup(func() { consistencyPollEvery = oldPoll })

	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Server{
		st: st, ctx: ctx,
		receivers:      map[int64]*receiverHandle{},
		finalizes:      map[int64]context.CancelFunc{},
		consistReq:     map[int64]bool{},
		consistDone:    map[int64]bool{},
		consistAt:      map[int64]time.Time{},
		quiesceErr:     map[int64]string{},
		pendingCutover: map[int64]api.FinalizeRequest{},
	}
	return s, ctx
}

func twoDiskMigration(t *testing.T, s *Server, ctx context.Context) api.Migration {
	t.Helper()
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "twodisk", SourceHostname: "twodisk", BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices: []api.DeviceSpec{
			{Device: "/dev/sda", SizeBytes: 25 << 30},
			{Device: "/dev/sdb", SizeBytes: 50 << 30},
		},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	for _, d := range m.Disks {
		if err := s.st.RecordAgentConnected(ctx, d.ID); err != nil {
			t.Fatalf("record agent connected: %v", err)
		}
	}
	m, err = s.st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload migration: %v", err)
	}
	return m
}

// TestQuiesceWaitsForEveryDiskBeforeFallingBack is the core fail-fast fix: one
// disk (the root) reporting it can't quiesce must NOT cut the wait short for
// the other disk — quiesceForCutover must keep waiting until every disk has
// reached a definite outcome (consistent OR failed) or the deadline passes.
func TestQuiesceWaitsForEveryDiskBeforeFallingBack(t *testing.T) {
	s, ctx := newQuiesceTestServer(t)
	m := twoDiskMigration(t, s, ctx)
	root, data := m.Disks[0], m.Disks[1]

	resultCh := make(chan bool, 1)
	go func() { resultCh <- s.quiesceForCutover(ctx, m) }()

	// Wait for both disks to be under an active consistency request.
	deadline := time.Now().Add(5 * time.Second)
	for !s.wantDiskConsistency(root.ID) || !s.wantDiskConsistency(data.ID) {
		if time.Now().After(deadline) {
			t.Fatal("quiesceForCutover never requested consistency on both disks")
		}
		time.Sleep(time.Millisecond)
	}

	// The root fails fast (the normal case: a running root almost never
	// remounts read-only).
	s.markDiskQuiesceFailed(root.ID, "could not remount / read-only")

	// The old fail-fast behavior would have returned here, before the data
	// disk gets any chance. Prove it does NOT: quiesceForCutover must still be
	// running a beat later, because the data disk hasn't settled yet.
	select {
	case v := <-resultCh:
		t.Fatalf("quiesceForCutover returned (%v) before the data disk reached a definite outcome — it must not fail fast on one disk", v)
	case <-time.After(60 * time.Millisecond):
	}

	// Now the data disk lands its crash-consistent pass.
	s.markDiskConsistent(data.ID)

	select {
	case got := <-resultCh:
		// Overall result stays false: NOT every disk delivered a consistent
		// pass (the return value's meaning is preserved).
		if got {
			t.Errorf("quiesceForCutover = true, want false (the root disk never delivered a consistent pass)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quiesceForCutover did not return after the data disk settled")
	}

	// One activity event must summarize BOTH disks by index and device path —
	// which captured a point-in-time snapshot and which did not — not a
	// per-disk immediate-return message.
	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var summary string
	for _, e := range evs {
		if strings.Contains(e.Message, "/dev/sda") && strings.Contains(e.Message, "/dev/sdb") {
			summary = e.Message
			break
		}
	}
	if summary == "" {
		t.Fatalf("expected one event naming both disks (index + device path), got events: %+v", evs)
	}
}

// TestQuiesceSucceedsWhenEveryDiskLandsConsistent is the happy path, now
// covering 2+ disks: only when ALL disks deliver a consistent pass does
// quiesceForCutover return true.
func TestQuiesceSucceedsWhenEveryDiskLandsConsistent(t *testing.T) {
	s, ctx := newQuiesceTestServer(t)
	m := twoDiskMigration(t, s, ctx)

	resultCh := make(chan bool, 1)
	go func() { resultCh <- s.quiesceForCutover(ctx, m) }()

	deadline := time.Now().Add(5 * time.Second)
	for !s.wantDiskConsistency(m.Disks[0].ID) || !s.wantDiskConsistency(m.Disks[1].ID) {
		if time.Now().After(deadline) {
			t.Fatal("quiesceForCutover never requested consistency on both disks")
		}
		time.Sleep(time.Millisecond)
	}
	s.markDiskConsistent(m.Disks[0].ID)
	time.Sleep(10 * time.Millisecond) // land at a different wall-clock moment
	s.markDiskConsistent(m.Disks[1].ID)

	select {
	case got := <-resultCh:
		if !got {
			t.Error("quiesceForCutover = false, want true when every disk lands a consistent pass")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quiesceForCutover did not return")
	}
}

// A single-disk migration's existing behavior (info-level report, no
// double-logged warn) must be unchanged by the multi-disk rework.
func TestQuiesceSingleDiskStillInfoNotWarnOnFailure(t *testing.T) {
	s, ctx := newQuiesceTestServer(t)
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "one", SourceHostname: "one", BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 25 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	if err := s.st.RecordAgentConnected(ctx, m.Disks[0].ID); err != nil {
		t.Fatalf("record agent: %v", err)
	}
	m, err = s.st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	resultCh := make(chan bool, 1)
	go func() { resultCh <- s.quiesceForCutover(ctx, m) }()
	deadline := time.Now().Add(5 * time.Second)
	for !s.wantDiskConsistency(m.Disks[0].ID) {
		if time.Now().After(deadline) {
			t.Fatal("quiesceForCutover never requested consistency")
		}
		time.Sleep(time.Millisecond)
	}
	s.markDiskQuiesceFailed(m.Disks[0].ID, "could not remount / read-only")

	select {
	case got := <-resultCh:
		if got {
			t.Error("quiesceForCutover = true, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quiesceForCutover did not return")
	}

	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawWarn bool
	for _, e := range evs {
		if e.Level == "warn" {
			sawWarn = true
		}
	}
	if sawWarn {
		t.Error("a single disk that can't quiesce is the normal case (a running root) and must not log a warn from quiesceForCutover itself")
	}
}
