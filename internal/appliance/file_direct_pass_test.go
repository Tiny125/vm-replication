package appliance

import (
	"context"
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/protocol"
)

// newFilePassMigration builds a direct file migration with a ready destination,
// the fixture every test in this file starts from.
func newFilePassMigration(t *testing.T, s *Server) (api.Migration, int64) {
	t.Helper()
	m, _, err := s.st.CreateMigration(context.Background(), api.CreateMigrationRequest{
		Name: "app", SourceHostname: "app", BootTarget: api.BootTargetFile,
		OSImage: "linode/ubuntu24.04", LinodeType: "g6-nanode-1",
		Devices: []api.DeviceSpec{{Device: "/", SizeBytes: 1 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	s.fileDests.Store(m.ID, &fileDest{instanceID: 100, ip: "192.0.2.9", ready: true, label: "app-dest", token: "tk"})
	return m, m.Disks[0].ID
}

func baselined(t *testing.T, s *Server, migID, diskID int64) bool {
	t.Helper()
	return s.diskBaselined(migID, diskID)
}

func eventsContaining(t *testing.T, s *Server, migID int64, sub string) int {
	t.Helper()
	evs, err := s.st.Events(context.Background(), migID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	n := 0
	for _, e := range evs {
		if strings.Contains(e.Message, sub) {
			n++
		}
	}
	return n
}

// In direct mode the appliance never sees the file data, so ONLY a pass the
// agent confirms may baseline the migration. This pins every guard in
// recordDirectFilePass: an old agent that reports nothing, a confirmed complete
// pass, a repeat of that same pass, a pass that landed on a different
// destination, and a pass that ended early.
func TestDirectPassReportBaselines(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)

	base := protocol.Hello{Mode: protocol.ModeFile, ReportsPasses: true}

	// 1. An older agent reports no pass at all: nothing may be recorded.
	old := protocol.Hello{Mode: protocol.ModeFile}
	s.recordDirectFilePass(m.ID, diskID, "/", old)
	if baselined(t, s, m.ID, diskID) {
		t.Fatal("an agent that reported no pass must not baseline the migration")
	}

	// 2. A confirmed COMPLETE pass to the destination we stream to: baseline.
	done := base
	done.LastPassSeq, done.LastPassComplete = 1, true
	done.LastPassEntries, done.LastPassBytes = 99030, 2<<30
	done.LastPassTarget = "192.0.2.9:5999"
	s.recordDirectFilePass(m.ID, diskID, "/", done)
	if !baselined(t, s, m.ID, diskID) {
		t.Fatal("a confirmed complete pass to the current destination must baseline the migration")
	}
	if n := eventsContaining(t, s, m.ID, "file copy complete"); n != 1 {
		t.Fatalf("want exactly 1 'file copy complete' event, got %d", n)
	}
	got, err := s.st.Migration(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	if got.Disks[0].BytesOnWire != int64(2<<30) {
		t.Fatalf("bytes_on_wire = %d, want the agent's reported %d", got.Disks[0].BytesOnWire, int64(2<<30))
	}
	if !cutoverReady(got) {
		t.Fatal("a baselined file migration must be cutover-ready")
	}

	// 3. The agent repeats its last pass in EVERY Hello — it must be credited once.
	s.recordDirectFilePass(m.ID, diskID, "/", done)
	if n := eventsContaining(t, s, m.ID, "file copy complete"); n != 1 {
		t.Fatalf("a repeated report must not be recorded twice; got %d completion events", n)
	}
}

// A pass that landed on a destination we are no longer streaming to proves
// nothing about the current one — a relaunched destination must earn its own
// baseline rather than inherit the old one's.
func TestDirectPassIgnoresStaleDestination(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)

	stale := protocol.Hello{Mode: protocol.ModeFile, ReportsPasses: true}
	stale.LastPassSeq, stale.LastPassComplete = 1, true
	stale.LastPassEntries, stale.LastPassBytes = 500, 1<<20
	stale.LastPassTarget = "198.51.100.1:5999" // a destination since replaced

	s.recordDirectFilePass(m.ID, diskID, "/", stale)
	if baselined(t, s, m.ID, diskID) {
		t.Fatal("a pass reported against a different destination must not baseline this one")
	}
}

// A pass that ended before the whole tree was walked has really moved data, but
// it is NOT the baseline: cutover needs one complete pass.
func TestDirectIncompletePassDoesNotBaseline(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)

	partial := protocol.Hello{Mode: protocol.ModeFile, ReportsPasses: true}
	partial.LastPassSeq, partial.LastPassComplete = 1, false
	partial.LastPassEntries, partial.LastPassBytes = 120, 4<<20
	partial.LastPassTarget = "192.0.2.9:5999"

	s.recordDirectFilePass(m.ID, diskID, "/", partial)
	if baselined(t, s, m.ID, diskID) {
		t.Fatal("an incomplete pass must not unlock cutover")
	}
	if n := eventsContaining(t, s, m.ID, "ended before the whole source tree"); n != 1 {
		t.Fatalf("want 1 warning about the early-ending pass, got %d", n)
	}
}

// The appliance-STAGING fallback (no destination instance) baselines through the
// receiver's onComplete instead. recordDirectFilePass must keep its hands off it,
// or a fallback migration would be double-counted.
func TestRecordDirectFilePassIgnoresFallback(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)
	s.fileDests.Delete(m.ID) // no destination => appliance-staging fallback

	h := protocol.Hello{Mode: protocol.ModeFile, ReportsPasses: true}
	h.LastPassSeq, h.LastPassComplete = 1, true
	h.LastPassEntries, h.LastPassBytes = 42, 1<<20
	h.LastPassTarget = "192.0.2.9:5999"

	s.recordDirectFilePass(m.ID, diskID, "/", h)
	if baselined(t, s, m.ID, diskID) {
		t.Fatal("the fallback path must baseline via onComplete, not via the pass report")
	}
}

// A block Hello must never be treated as a file pass report.
func TestRecordDirectFilePassIgnoresBlockSessions(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)

	blockHello := protocol.Hello{ // no Mode => block session
		ReportsPasses: true, LastPassSeq: 1, LastPassComplete: true,
		LastPassEntries: 10, LastPassBytes: 1 << 20, LastPassTarget: "192.0.2.9:5999",
	}
	s.recordDirectFilePass(m.ID, diskID, "/dev/sda", blockHello)
	if baselined(t, s, m.ID, diskID) {
		t.Fatal("a block session must not baseline through the file pass reporter")
	}
}

// While the first pass is still streaming the operator must be told so — a bare
// "copying files" reads like progress and invites a premature power-off. An
// agent too old to confirm must say what to do about it.
func TestFileCopyDetailIsHonestWhileStreaming(t *testing.T) {
	s := automationServer(t)
	m, diskID := newFilePassMigration(t, s)
	got, err := s.st.Migration(context.Background(), m.ID)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}

	// Streaming, agent confirms passes: say the pass must finish first.
	s.noteDirectStreaming(m.ID, diskID, "192.0.2.9:5999", protocol.Hello{Mode: protocol.ModeFile, ReportsPasses: true})
	if d := s.fileCopyDetail(got, false); !strings.Contains(d, "must finish before cutover") {
		t.Fatalf("streaming detail should say the pass must finish, got %q", d)
	}

	// An agent that cannot confirm: tell the operator to re-run enrollment.
	s.directPasses.Delete(diskID)
	s.noteDirectStreaming(m.ID, diskID, "192.0.2.9:5999", protocol.Hello{Mode: protocol.ModeFile})
	if d := s.fileCopyDetail(got, false); !strings.Contains(d, "re-run the enrollment command") {
		t.Fatalf("old-agent detail should tell the operator to re-enroll, got %q", d)
	}

	// Baselined: the original wording.
	if d := s.fileCopyDetail(got, true); d != "files copied" {
		t.Fatalf("baselined detail = %q, want %q", d, "files copied")
	}
}
