package appliance

import (
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
)

// canCloseMigration gates the "Close migration" action (finish a successful
// migration): only a launched or image-ready migration may be closed, which
// deletes the temporary vmrep- replication volume while keeping the launched
// instance and its cutover clone volumes. Any in-progress or failed migration
// must be deleted (not closed) instead.
func TestCanCloseMigration(t *testing.T) {
	closable := []api.MigrationState{api.MigImageReady, api.MigLaunched}
	for _, st := range closable {
		if !canCloseMigration(st) {
			t.Errorf("state %q should be closable", st)
		}
	}
	notClosable := []api.MigrationState{
		api.MigCreated, api.MigAwaitingAgent, api.MigReplicating,
		api.MigReady, api.MigAwaitingCutover, api.MigMigrating, api.MigFailed,
	}
	for _, st := range notClosable {
		if canCloseMigration(st) {
			t.Errorf("state %q should not be closable", st)
		}
	}
}

// activeMigrationCount counts migrations that are still in progress (created or
// running). A "migration complete" (launched/image_ready) or failed migration
// does not count as active, so account-level actions (remove token, delete
// bucket) are allowed once every remaining migration has finished.
func TestActiveMigrationCount(t *testing.T) {
	migs := []api.Migration{
		{State: api.MigReplicating},   // active
		{State: api.MigLaunched},      // complete — not active
		{State: api.MigImageReady},    // complete — not active
		{State: api.MigAwaitingAgent}, // active
		{State: api.MigFailed},        // not active
	}
	if got, want := activeMigrationCount(migs), 2; got != want {
		t.Errorf("activeMigrationCount = %d, want %d", got, want)
	}
	// All completed → nothing active → token/bucket removal allowed.
	done := []api.Migration{{State: api.MigLaunched}, {State: api.MigImageReady}}
	if got := activeMigrationCount(done); got != 0 {
		t.Errorf("activeMigrationCount(all complete) = %d, want 0", got)
	}
	if got := activeMigrationCount(nil); got != 0 {
		t.Errorf("activeMigrationCount(nil) = %d, want 0", got)
	}
}
