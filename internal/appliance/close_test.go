package appliance

import (
	"os"
	"strings"
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

// F-21. Deleting a migration only DETACHED its replication volume
// (cleanupOpts{keepReplVolume: true}) while the console told the operator:
//
//	"Migration #N deleted. The replication volume and data were removed."
//
// The volume was still in the account, still billing, and now owned by nothing
// — the migration row it belonged to was gone, so no retry could ever reuse it.
// A silent leak is bad; a leak the UI actively denies is worse, because it
// stops anyone going to look.
//
// Delete now removes the volume, matching what the operator is told. Close
// already did this correctly.
func TestDeleteMigrationRemovesTheReplicationVolume(t *testing.T) {
	src, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (s *Server) handleDeleteMigration")
	if i < 0 {
		t.Fatal("handleDeleteMigration not found")
	}
	body := s[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "keepReplVolume: true") {
		t.Error("Delete still keeps the replication volume, but the console tells the operator it was removed — the volume keeps billing with no migration left to use it")
	}
}

// The console's post-delete message and the code must agree. If someone ever
// makes Delete keep the volume again, this says the message has to change too.
func TestDeleteMessageMatchesWhatDeleteDoes(t *testing.T) {
	if !strings.Contains(consoleHTML, "The replication volume and data were removed") {
		t.Skip("post-delete wording changed; re-check it against handleDeleteMigration")
	}
	src, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (s *Server) handleDeleteMigration")
	body := s[i:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if strings.Contains(body, "keepReplVolume") && strings.Contains(body, "true") {
		t.Error("the console claims the replication volume was removed; Delete must actually remove it")
	}
}
