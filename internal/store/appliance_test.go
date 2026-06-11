package store

import (
	"context"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

func TestAdminPassword(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if set, _ := st.AdminPasswordSet(ctx); set {
		t.Fatal("password should be unset initially")
	}
	if err := st.SetAdminPassword(ctx, "s3cret-pw"); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	if set, _ := st.AdminPasswordSet(ctx); !set {
		t.Fatal("password should be set")
	}
	ok, err := st.VerifyAdminPassword(ctx, "s3cret-pw")
	if err != nil || !ok {
		t.Fatalf("correct password rejected (ok=%v err=%v)", ok, err)
	}
	if ok, _ := st.VerifyAdminPassword(ctx, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestLinodeTokenRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	st.SetEncryptionKey(make([]byte, 32)) // deterministic test key

	if set, _ := st.LinodeTokenSet(ctx); set {
		t.Fatal("token should be unset initially")
	}
	if err := st.SetLinodeToken(ctx, "abc123-linode-token"); err != nil {
		t.Fatalf("SetLinodeToken: %v", err)
	}
	got, err := st.LinodeToken(ctx)
	if err != nil {
		t.Fatalf("LinodeToken: %v", err)
	}
	if got != "abc123-linode-token" {
		t.Fatalf("token round-trip mismatch: %q", got)
	}
	// The stored value must not be the plaintext.
	raw, _, _ := st.GetSetting(ctx, keyLinodeTok)
	if raw == "abc123-linode-token" {
		t.Fatal("token stored in plaintext")
	}
}

func TestMigrationLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	m, token, err := st.CreateMigration(ctx, api.CreateMigrationRequest{
		Name: "web01", SourceHostname: "web01",
		Devices: []api.DeviceSpec{
			{Device: "/dev/sda", SizeBytes: 80 << 30},
			{Device: "/dev/sdb", SizeBytes: 200 << 30},
		},
	})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if token == "" || m.State != api.MigCreated {
		t.Fatalf("unexpected create result: token=%q state=%q", token, m.State)
	}
	if len(m.Disks) != 2 || m.Disks[0].SourceDevice != "/dev/sda" || m.Disks[1].Index != 1 {
		t.Fatalf("disks not created correctly: %+v", m.Disks)
	}

	// Token lookup resolves to the same migration (with disks).
	byTok, err := st.MigrationByToken(ctx, token)
	if err != nil || byTok.ID != m.ID || len(byTok.Disks) != 2 {
		t.Fatalf("MigrationByToken: %v (id %d vs %d, disks %d)", err, byTok.ID, m.ID, len(byTok.Disks))
	}
	if _, err := st.MigrationByToken(ctx, "bogus"); err == nil {
		t.Fatal("bogus token should not resolve")
	}

	// A completed full sync on disk 0 advances migration state and the disk.
	if err := st.RecordDiskSync(ctx, m.ID, m.Disks[0].ID, true, 100, 100, 1<<20); err != nil {
		t.Fatalf("RecordDiskSync: %v", err)
	}
	got, _ := st.Migration(ctx, m.ID)
	if !got.Disks[0].FullSyncDone {
		t.Error("disk 0 full_sync_done should be true")
	}
	if got.Disks[1].FullSyncDone {
		t.Error("disk 1 should not be done yet")
	}
	if got.State != api.MigReplicating {
		t.Errorf("state = %q, want replicating", got.State)
	}
	if got.Disks[0].TotalBlocks != 100 || got.Disks[0].LastSyncAt.IsZero() {
		t.Errorf("disk 0 progress not recorded: %+v", got.Disks[0])
	}

	// A later delta keeps full_sync_done true.
	_ = st.RecordDiskSync(ctx, m.ID, m.Disks[0].ID, false, 100, 3, 12<<20)
	got, _ = st.Migration(ctx, m.ID)
	if !got.Disks[0].FullSyncDone {
		t.Error("full_sync_done must remain true after a delta")
	}

	// Deleting the migration cascades to its disks.
	if err := st.DeleteMigration(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMigration: %v", err)
	}
	if _, err := st.Migration(ctx, m.ID); err == nil {
		t.Error("migration should be gone after delete")
	}
}
