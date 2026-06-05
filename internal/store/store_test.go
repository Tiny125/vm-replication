package store

import (
	"context"
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertServerIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	a, err := st.UpsertServer(ctx, api.RegisterServerRequest{Name: "web01", Role: api.RoleSource, Device: "/dev/sda"})
	if err != nil {
		t.Fatalf("UpsertServer: %v", err)
	}
	b, err := st.UpsertServer(ctx, api.RegisterServerRequest{Name: "web01", Role: api.RoleSource, Device: "/dev/vda"})
	if err != nil {
		t.Fatalf("UpsertServer (update): %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("expected same id on upsert, got %d then %d", a.ID, b.ID)
	}
	if b.Device != "/dev/vda" {
		t.Errorf("device not updated: %q", b.Device)
	}
	servers, _ := st.ListServers(ctx)
	if len(servers) != 1 {
		t.Errorf("expected 1 server, got %d", len(servers))
	}
}

func TestJobLifecycleAndRPO(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	job, err := st.CreateJob(ctx, api.CreateJobRequest{Name: "mig-web01", TargetAddr: "1.2.3.4:4444", RPOTargetSec: 60})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.State != api.JobActive {
		t.Errorf("new job state = %q, want active", job.State)
	}

	// No syncs yet: RPO should be breached (target set, nothing replicated).
	now := time.Now()
	st0, _ := st.JobStatus(ctx, job, now)
	if !st0.RPOBreached {
		t.Errorf("expected RPO breached before any sync")
	}

	// A successful sync 10s ago → RPO ~10s, under the 60s target.
	_, err = st.AddSync(ctx, job.ID, api.ReportSyncRequest{
		Mode: api.SyncFull, StartedAt: now.Add(-40 * time.Second), FinishedAt: now.Add(-10 * time.Second),
		TotalBlocks: 100, ChangedBlocks: 100, BytesOnWire: 1 << 20, DurationMS: 30000, OK: true,
	})
	if err != nil {
		t.Fatalf("AddSync: %v", err)
	}
	st1, _ := st.JobStatus(ctx, job, now)
	if st1.RPOBreached {
		t.Errorf("RPO should not be breached at ~10s with 60s target (rpo=%.0f)", st1.RPOSeconds)
	}
	if st1.RPOSeconds < 9 || st1.RPOSeconds > 11 {
		t.Errorf("RPOSeconds = %.1f, want ~10", st1.RPOSeconds)
	}
	if st1.TotalSyncs != 1 {
		t.Errorf("TotalSyncs = %d, want 1", st1.TotalSyncs)
	}

	// A failed sync should not advance the OK timestamp.
	_, _ = st.AddSync(ctx, job.ID, api.ReportSyncRequest{
		Mode: api.SyncDelta, StartedAt: now, FinishedAt: now, OK: false, Error: "boom",
	})
	st2, _ := st.JobStatus(ctx, job, now)
	if st2.LastSync == nil || st2.LastSync.OK {
		t.Errorf("last sync should be the failed one")
	}
	if st2.LastOKSync == nil || !st2.LastOKSync.OK {
		t.Errorf("last OK sync should still be the earlier successful one")
	}

	// State transition.
	if err := st.SetJobState(ctx, job.ID, api.JobCutover); err != nil {
		t.Fatalf("SetJobState: %v", err)
	}
	got, _ := st.Job(ctx, job.ID)
	if got.State != api.JobCutover {
		t.Errorf("state = %q, want cutover", got.State)
	}
}

func TestAddSyncUnknownJob(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.AddSync(context.Background(), 999, api.ReportSyncRequest{OK: true}); err == nil {
		t.Error("expected error adding sync to unknown job")
	}
}
