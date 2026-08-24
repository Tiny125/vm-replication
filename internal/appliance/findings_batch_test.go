package appliance

import (
	"testing"
	"time"

	"github.com/tiny125/vm-replication/internal/api"
)

// F-10. A migration that has finished successfully must not display a red
// checklist. The pre-flight checks measure LIVE state — is the agent checking
// in, is replication lag inside the target — and after cutover all of that is
// correctly false: the operator was told to remove the agent, replication is
// stopped, and the lag clock keeps running unbounded. Reporting them as
// FAILURES on a completed migration tells the operator their successful
// migration broke.
func TestValidationsOnFinishedMigrationAreNotFailures(t *testing.T) {
	s := &Server{cfg: Config{RPOTargetSec: 120}}
	m := api.Migration{
		State:      api.MigLaunched,
		BootTarget: api.BootTargetVolume,
		Disks: []api.Disk{{
			ID: 1, Index: 0, VolumeDevice: "/dev/sdc",
			FullSyncDone: true,
			// Deliberately stale: the agent was removed at cutover and the last
			// sync was ages ago. This is the normal state of a finished migration.
			AgentLastSeen: time.Now().Add(-3 * time.Hour),
			LastSyncAt:    time.Now().Add(-3 * time.Hour),
		}},
	}
	for _, v := range s.validations(m, 10800) {
		if !v.OK {
			t.Errorf("finished migration still reports FAILING check %q (%s) — a completed migration must not show a red checklist", v.Name, v.Detail)
		}
	}
}

// The same checks must still be able to FAIL while the migration is live —
// otherwise the fix has simply blinded the pre-flight checklist.
func TestValidationsStillFailWhileReplicating(t *testing.T) {
	s := &Server{cfg: Config{RPOTargetSec: 120}}
	m := api.Migration{
		State:      api.MigReplicating,
		BootTarget: api.BootTargetVolume,
		Disks: []api.Disk{{
			ID: 1, Index: 0, VolumeDevice: "/dev/sdc",
			AgentLastSeen: time.Now().Add(-3 * time.Hour), // agent gone: a real problem now
		}},
	}
	var sawFail bool
	for _, v := range s.validations(m, 10800) {
		if !v.OK {
			sawFail = true
		}
	}
	if !sawFail {
		t.Error("a replicating migration with a dead agent must still report a failing check")
	}
}

// migrationFinished is the predicate the fix turns on. Cutover-in-progress is
// NOT finished: the checklist is still meaningful while the image is captured.
func TestMigrationFinished(t *testing.T) {
	finished := map[api.MigrationState]bool{
		api.MigLaunched:        true,
		api.MigCreated:         false,
		api.MigAwaitingAgent:   false,
		api.MigReplicating:     false,
		api.MigReady:           false,
		api.MigAwaitingCutover: false,
		api.MigMigrating:       false,
		api.MigFailed:          false,
	}
	for st, want := range finished {
		if got := migrationFinished(api.Migration{State: st}); got != want {
			t.Errorf("migrationFinished(%s) = %v, want %v", st, got, want)
		}
	}
}
