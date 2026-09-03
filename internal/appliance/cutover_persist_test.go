package appliance

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
	"github.com/tiny125/vm-replication/internal/store"
)

// F-24: the rescue-mode cutover token and the Lish copy command used to live
// only in the appliance's memory (cutoverStreams / cutoverCmds), so
// restarting applianced while a migration sat in `migrating` waiting for the
// operator to paste the copy command dropped both. Measured live
// (findings.md F-24):
//
//	state: migrating | phase: finalizing (convert + clone)
//	copy cmd present: False        <- the card no longer shows a command
//	launched id: 103564624         <- the rescue instance is still running
//
// leaving the migration unfinishable with nothing explaining why. These tests
// cover persisting the stream to the settings store and restoring it at
// startup (1a), and the startup safety-net event when nothing can be
// restored (1b).

// testPersistStore opens a fresh in-memory store for these tests.
func testPersistStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newDiskBootMigration(t *testing.T, st *store.Store, ctx context.Context, name string) api.Migration {
	t.Helper()
	m, _, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: name, SourceHostname: name, BootTarget: api.BootTargetDisk,
		LinodeType: "g6-nanode-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 25 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	return m
}

// TestCutoverStreamRoundTripsAcrossRestart: register a stream + copy command
// on one Server (simulating the running appliance), then rebuild a FRESH
// Server backed by the same store (simulating a restart) and confirm the
// token resolves and the copy command is back.
func TestCutoverStreamRoundTripsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "rt")
	if err := st.SetMigrationState(ctx, m.ID, api.MigMigrating, ""); err != nil {
		t.Fatalf("set state: %v", err)
	}

	s1 := &Server{st: st, ctx: ctx}
	tok := s1.registerCutoverStream(m.ID, "/dev/some-volume", 12345, time.Hour)
	s1.setCutoverCopyCmd(m.ID, "curl … | sh")

	// A brand-new Server sharing only the store — what a restart looks like.
	s2 := &Server{st: st, ctx: ctx}
	if !s2.restoreCutoverStream(m.ID) {
		t.Fatal("restoreCutoverStream must succeed for a persisted, unexpired entry")
	}
	stream, ok := s2.lookupCutoverStream(tok)
	if !ok {
		t.Fatal("the restored token must resolve")
	}
	if stream.migID != m.ID || stream.path != "/dev/some-volume" || stream.bytes != 12345 {
		t.Errorf("restored stream = %+v, want migID=%d path=/dev/some-volume bytes=12345", stream, m.ID)
	}
	if got := s2.cutoverCopyCmdFor(m.ID); got != "curl … | sh" {
		t.Errorf("restored copy command = %q, want the original", got)
	}
}

// TestExpiredCutoverStreamNotRestored: an entry whose persisted expiry has
// already passed must NOT be restored — a restored-but-dead token would just
// 403 the moment it's pasted — and the stale persisted entry must be cleared.
func TestExpiredCutoverStreamNotRestored(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "exp")

	s1 := &Server{st: st, ctx: ctx}
	tok := s1.registerCutoverStream(m.ID, "/dev/x", 1, -time.Second) // already expired
	s1.setCutoverCopyCmd(m.ID, "curl … | sh")

	s2 := &Server{st: st, ctx: ctx}
	if s2.restoreCutoverStream(m.ID) {
		t.Fatal("an expired persisted entry must not be restored")
	}
	if _, ok := s2.lookupCutoverStream(tok); ok {
		t.Error("an expired token must not resolve after restore")
	}
	if v, ok, err := st.GetSetting(ctx, cutoverStreamSettingKey(m.ID)); err != nil || ok {
		t.Errorf("the expired persisted entry must be cleared, got ok=%v value=%q err=%v", ok, v, err)
	}
}

// TestDropCutoverStreamClearsPersistedCopy: dropCutoverStream must remove the
// durable copy too, or a later restart would resurrect a cutover that already
// finished, failed, or was cancelled.
func TestDropCutoverStreamClearsPersistedCopy(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "drop")

	s := &Server{st: st, ctx: ctx}
	s.registerCutoverStream(m.ID, "/dev/x", 1, time.Hour)
	s.setCutoverCopyCmd(m.ID, "curl … | sh")
	if _, ok, _ := st.GetSetting(ctx, cutoverStreamSettingKey(m.ID)); !ok {
		t.Fatal("setup: expected a persisted entry before dropping")
	}
	s.dropCutoverStream(m.ID)
	if _, ok, _ := st.GetSetting(ctx, cutoverStreamSettingKey(m.ID)); ok {
		t.Error("dropCutoverStream must delete the persisted copy")
	}
}

// TestRestartRestoresPendingCopyCommand exercises StartActiveReceivers end to
// end: a migration parked in `migrating` with a persisted, unexpired stream
// must come back with its stream restored AND get an activity event telling
// the operator the command is back (in the tone of the existing restart
// message at server.go:617-620).
func TestRestartRestoresPendingCopyCommand(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "res")
	if err := st.SetMigrationImage(ctx, m.ID, "", 103564624); err != nil {
		t.Fatalf("set image: %v", err)
	}
	if err := st.SetMigrationState(ctx, m.ID, api.MigMigrating, ""); err != nil {
		t.Fatalf("set state: %v", err)
	}

	s1 := &Server{st: st, ctx: ctx}
	tok := s1.registerCutoverStream(m.ID, "/dev/y", 999, time.Hour)
	s1.setCutoverCopyCmd(m.ID, "curl -fsSL … | sh")

	s2 := &Server{st: st, ctx: ctx}
	s2.StartActiveReceivers()

	if _, ok := s2.lookupCutoverStream(tok); !ok {
		t.Fatal("the cutover token must be restored after a simulated restart")
	}
	if got := s2.cutoverCopyCmdFor(m.ID); got == "" {
		t.Error("the copy command must be restored")
	}
	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawRestore bool
	for _, e := range evs {
		if strings.Contains(e.Message, "restarted") && strings.Contains(e.Message, "restored") {
			sawRestore = true
		}
	}
	if !sawRestore {
		t.Errorf("restoring the copy command should be announced on the migration's activity log, got events: %+v", evs)
	}
}

// TestRestartWithNoRestorableStreamWarnsWithRecoveryAction is the safety net
// (1b): a migration mid-cutover with a launched rescue instance but nothing
// to restore (never persisted yet, or persisted-but-expired) must get an
// activity event naming the situation AND the exact recovery action — Stop,
// then Start again — instead of leaving the operator to discover the
// vanished command on their own.
func TestRestartWithNoRestorableStreamWarnsWithRecoveryAction(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "strand")
	if err := st.SetMigrationImage(ctx, m.ID, "", 103564624); err != nil { // rescue instance is running
		t.Fatalf("set image: %v", err)
	}
	if err := st.SetMigrationState(ctx, m.ID, api.MigMigrating, ""); err != nil {
		t.Fatalf("set state: %v", err)
	}
	// No cutover stream was ever registered on this store — simulates either
	// "never issued yet" or "expired while the service was down".

	s := &Server{st: st, ctx: ctx}
	s.StartActiveReceivers()

	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var sawRecovery bool
	for _, e := range evs {
		low := strings.ToLower(e.Message)
		if strings.Contains(low, "restart") && strings.Contains(low, "stop") && strings.Contains(low, "start") {
			sawRecovery = true
		}
	}
	if !sawRecovery {
		t.Errorf("expected a recovery event naming Stop-then-Start, got events: %+v", evs)
	}
}

// A migration still mid-cutover but with NO launched instance yet (e.g.
// applianced restarted before the rescue instance was even created) must not
// get the "stranded" warning — there's no live rescue instance to strand.
func TestRestartNoSafetyNetWithoutLaunchedInstance(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m := newDiskBootMigration(t, st, ctx, "early")
	if err := st.SetMigrationState(ctx, m.ID, api.MigMigrating, ""); err != nil {
		t.Fatalf("set state: %v", err)
	}
	s := &Server{st: st, ctx: ctx}
	s.StartActiveReceivers()
	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		low := strings.ToLower(e.Message)
		if strings.Contains(low, "stop") && strings.Contains(low, "start") {
			t.Errorf("no launched instance yet — must not warn about a stranded cutover, got: %s", e.Message)
		}
	}
}

// A volume-boot migration must never trigger the disk-boot safety net — it
// never uses a rescue-mode cutover stream in the first place.
func TestRestartSafetyNetIgnoresVolumeBoot(t *testing.T) {
	ctx := context.Background()
	st := testPersistStore(t)
	m, _, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "vol", SourceHostname: "vol", BootTarget: api.BootTargetVolume,
		LinodeType: "g6-nanode-1",
		Devices:    []api.DeviceSpec{{Device: "/dev/sda", SizeBytes: 25 << 30}},
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	if err := st.SetMigrationImage(ctx, m.ID, "", 555); err != nil {
		t.Fatalf("set image: %v", err)
	}
	if err := st.SetMigrationState(ctx, m.ID, api.MigMigrating, ""); err != nil {
		t.Fatalf("set state: %v", err)
	}
	s := &Server{st: st, ctx: ctx}
	s.StartActiveReceivers()
	evs, err := st.Events(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	for _, e := range evs {
		if strings.Contains(e.Message, "Stop") && strings.Contains(e.Message, "Start") {
			t.Errorf("volume-boot migration must not get the disk-boot recovery event, got: %s", e.Message)
		}
	}
}
