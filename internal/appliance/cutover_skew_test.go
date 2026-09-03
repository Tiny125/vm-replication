package appliance

import (
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// F-14 (2b): measured live, a two-disk source's disks land their final
// cutover passes ~54s apart (findings.md), so the destination's disks can
// reflect different instants. These tests cover recording the wall-clock
// spread across disks and surfacing it on the migration API/activity log —
// and that a single-disk migration, which structurally has no skew, never
// shows a meaningless "0s apart".

// TestCutoverSkewRecordedAndExposedForMultiDisk drives a two-disk quiesce
// where the disks land their consistent passes at different times and checks
// the measured spread shows up both as an activity event and on the
// migration's API view.
func TestCutoverSkewRecordedAndExposedForMultiDisk(t *testing.T) {
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
	time.Sleep(60 * time.Millisecond) // land at a measurably different instant
	s.markDiskConsistent(m.Disks[1].ID)

	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("quiesceForCutover did not return")
	}

	v := s.view(ctx, m, "")
	if v.CutoverSkewSeconds <= 0 {
		t.Errorf("CutoverSkewSeconds = %v, want > 0 after disks landed ~60ms apart", v.CutoverSkewSeconds)
	}

	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawSkewEvent bool
	for _, e := range evs {
		if strings.Contains(e.Message, "apart") {
			sawSkewEvent = true
		}
	}
	if !sawSkewEvent {
		t.Errorf("expected an activity event reporting the measured skew ('...apart'), got events: %+v", evs)
	}
}

// TestSingleDiskMigrationHasNoSkew: a single-disk migration structurally
// cannot have skew between disks — it must not report a meaningless "0s
// apart" event, and the API field must stay unset/zero.
func TestSingleDiskMigrationHasNoSkew(t *testing.T) {
	s, ctx := newQuiesceTestServer(t)
	m, _, err := s.st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "skew1", SourceHostname: "skew1", BootTarget: api.BootTargetDisk,
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
	s.markDiskConsistent(m.Disks[0].ID)
	select {
	case <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("quiesceForCutover did not return")
	}

	v := s.view(ctx, m, "")
	if v.CutoverSkewSeconds != 0 {
		t.Errorf("CutoverSkewSeconds = %v, want 0 for a single-disk migration", v.CutoverSkewSeconds)
	}
	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if strings.Contains(e.Message, "apart") {
			t.Errorf("single-disk migration must not report a skew event, got: %s", e.Message)
		}
	}
}

// TestCutoverSkewReportedWhenOnlyOneDiskQuiesces covers the case the skew
// metric actually exists for. The code itself documents that a running root
// "almost never remounts read-only", so the ordinary multi-disk shape is: the
// root REFUSES to quiesce while the separate data volume quiesces cleanly.
//
// Measuring the spread only across disks that landed a CONSISTENT capture
// misses exactly that shape — one disk has a capture timestamp, the other has
// none, and no skew is reported at all. But the destination's disks still
// reflect different instants: the refusing disk contributes whatever its last
// applied pass happened to be. That is the 54s gap measured live
// (findings.md, F-14), and it is the dangerous case, not the safe one.
//
// So a disk with no consistent capture must fall back to its last applied
// pass (api.Disk.LastSyncAt) rather than being skipped.
func TestCutoverSkewReportedWhenOnlyOneDiskQuiesces(t *testing.T) {
	s, ctx := newQuiesceTestServer(t)
	m := twoDiskMigration(t, s, ctx)

	// Disk 0 (the root) reported a while ago and then refused to quiesce;
	// disk 1 (the data volume) captures a consistent pass now.
	if err := s.st.RecordDiskSync(ctx, m.ID, m.Disks[0].ID, true, 100, 0, 0); err != nil {
		t.Fatalf("record disk sync: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	s.markDiskQuiesceFailed(m.Disks[0].ID, "mount: /: mount point is busy")
	s.markDiskConsistent(m.Disks[1].ID)

	m, err := s.st.Migration(ctx, m.ID)
	if err != nil {
		t.Fatalf("reload migration: %v", err)
	}
	s.recordCutoverSkew(m)

	v := s.view(ctx, m, "")
	if v.CutoverSkewSeconds <= 0 {
		t.Errorf("CutoverSkewSeconds = %v, want > 0: the root refused to quiesce and the "+
			"data volume captured later, so the destination's disks reflect different "+
			"instants — this is the case the metric exists for", v.CutoverSkewSeconds)
	}

	evs, err := s.st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawSkew bool
	for _, e := range evs {
		if strings.Contains(e.Message, "apart") {
			sawSkew = true
		}
	}
	if !sawSkew {
		t.Error("no skew event logged when only one disk quiesced — the operator gets no " +
			"warning in precisely the shape that carries the risk")
	}
}
