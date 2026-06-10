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
		Name: "web01", SourceHostname: "web01", SourceDevice: "/dev/sda", SourceDiskSize: 80 << 30,
	})
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if token == "" || m.State != api.MigCreated {
		t.Fatalf("unexpected create result: token=%q state=%q", token, m.State)
	}

	// Token lookup resolves to the same migration.
	byTok, err := st.MigrationByToken(ctx, token)
	if err != nil || byTok.ID != m.ID {
		t.Fatalf("MigrationByToken: %v (id %d vs %d)", err, byTok.ID, m.ID)
	}
	if _, err := st.MigrationByToken(ctx, "bogus"); err == nil {
		t.Fatal("bogus token should not resolve")
	}

	// A completed full sync advances state and sets full_sync_done.
	if err := st.RecordMigrationSync(ctx, m.ID, true, 100, 100, 1<<20); err != nil {
		t.Fatalf("RecordMigrationSync: %v", err)
	}
	got, _ := st.Migration(ctx, m.ID)
	if !got.FullSyncDone {
		t.Error("full_sync_done should be true")
	}
	if got.State != api.MigReplicating {
		t.Errorf("state = %q, want replicating", got.State)
	}
	if got.TotalBlocks != 100 || got.LastSyncAt.IsZero() {
		t.Errorf("progress not recorded: %+v", got)
	}

	// A later delta keeps full_sync_done true.
	_ = st.RecordMigrationSync(ctx, m.ID, false, 100, 3, 12<<20)
	got, _ = st.Migration(ctx, m.ID)
	if !got.FullSyncDone {
		t.Error("full_sync_done must remain true after a delta")
	}
}
