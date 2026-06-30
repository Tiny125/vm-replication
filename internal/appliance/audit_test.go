package appliance

import (
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

// isMigrationActive gates destructive account-level actions (e.g. deleting the
// audit bucket): only finished (image_ready/launched) or failed migrations are
// inactive; everything else counts as created/running.
func TestIsMigrationActive(t *testing.T) {
	active := []api.MigrationState{
		api.MigCreated, api.MigAwaitingAgent, api.MigReplicating,
		api.MigReady, api.MigAwaitingCutover, api.MigMigrating,
	}
	for _, st := range active {
		if !isMigrationActive(st) {
			t.Errorf("state %q should be active", st)
		}
	}
	inactive := []api.MigrationState{api.MigImageReady, api.MigLaunched, api.MigFailed}
	for _, st := range inactive {
		if isMigrationActive(st) {
			t.Errorf("state %q should be inactive", st)
		}
	}
}

// auditBucketName drops the old "-NN" suffix: it is exactly vmrep-audit-<id>.
func TestAuditBucketName(t *testing.T) {
	s := &Server{}
	s.cfg.ApplianceLinodeID = 99334138
	if got, want := s.auditBucketName(), "vmrep-audit-99334138"; got != want {
		t.Errorf("auditBucketName() = %q, want %q", got, want)
	}
	s.cfg.ApplianceLinodeID = 0
	if got, want := s.auditBucketName(), "vmrep-audit"; got != want {
		t.Errorf("auditBucketName() off-Linode = %q, want %q", got, want)
	}
}
