package appliance

import (
	"testing"

	"github.com/tiny125/vm-replication/internal/protocol"
)

const gib = int64(1 << 30)

// deviceSizeMismatch is the wrong-source-disk guard: when the agent's Hello
// declares a device size grossly different from the size the migration was
// created with, the agent is reading the WRONG DISK (migration #1 "game2": the
// migration declared 80 GiB but the agent's /dev/sda was a ~512 MiB swap disk,
// so the "full sync" was 496 MiB of swap and cutover failed at convert). The
// tolerance must stay generous — operators enter sizes in whole GB rounded up
// and disks are often a little smaller than nominal — so only gross mismatches
// (reported less than half or more than double the declared size) reject.
func TestDeviceSizeMismatch(t *testing.T) {
	cases := []struct {
		name               string
		declared, reported int64
		want               bool
	}{
		{"game2: 512MiB swap disk vs 80GiB declared", 80 * gib, 512 << 20, true},
		{"tiny data disk vs big declared", 100 * gib, 8 * gib, true},
		{"agent disk double the declared size", 40 * gib, 100 * gib, true},
		{"exact match", 80 * gib, 80 * gib, false},
		{"rounded up by operator (79.5GiB real)", 80 * gib, 80*gib - 512<<20, false},
		{"declared slightly small", 80 * gib, 85 * gib, false},
		{"half exactly is still accepted", 80 * gib, 40 * gib, false},
		{"declared unknown (0) — no check", 0, 512 << 20, false},
		{"reported unknown (0) — no check", 80 * gib, 0, false},
	}
	for _, c := range cases {
		if got := deviceSizeMismatch(c.declared, c.reported); got != c.want {
			t.Errorf("%s: deviceSizeMismatch(%d, %d) = %v, want %v", c.name, c.declared, c.reported, got, c.want)
		}
	}
}

// checkAgentHello is the per-session identity + geometry guard. Identity: agent
// certs are GLOBAL (every source ever enrolled keeps valid mTLS credentials) and
// receiver ports repeat across appliance reinstalls (port = 5000 + disk id, ids
// restart with a fresh DB), so an old, never-uninstalled agent on some other
// machine can connect to a NEW migration's port and stream its disk into the
// fresh volume (migration #1 "game2": a rogue ~512 MiB swap disk arrived as the
// "initial full sync" of an 80 GiB migration). Each enrollment therefore carries
// a unique job id (the enrollment-token prefix) and any session whose job id
// doesn't match is rejected.
func TestCheckAgentHello(t *testing.T) {
	const job = "tok0123456789abc"
	mk := func(jobID string, size int64) protocol.Hello {
		return protocol.Hello{JobID: jobID, SourceHostname: "some-host", DevicePath: "/dev/sda", DeviceSize: size}
	}

	// Wrong job id — a rogue/stale agent (pre-update agents send "default").
	for _, bad := range []string{"default", "othertoken000000", ""} {
		if err := checkAgentHello(job, 80*gib, 0, "/dev/sda", mk(bad, 80*gib)); err == nil {
			t.Errorf("job %q: expected rejection for a non-matching job id", bad)
		}
	}
	// Matching job id and sane size — accepted.
	if err := checkAgentHello(job, 80*gib, 0, "/dev/sda", mk(job, 80*gib)); err != nil {
		t.Errorf("matching job id rejected: %v", err)
	}
	// Matching job id but wrong-sized device — still rejected (size guard).
	if err := checkAgentHello(job, 80*gib, 0, "/dev/sda", mk(job, 512<<20)); err == nil {
		t.Error("expected rejection for a grossly mismatched device size")
	}
	// Quiesce-failure report: correct job but no geometry (size 0) — accepted
	// (it carries no data; the size guard must not false-reject it).
	if err := checkAgentHello(job, 80*gib, 0, "/dev/sda", mk(job, 0)); err != nil {
		t.Errorf("zero-size (quiesce report) rejected: %v", err)
	}
	// No expected job (e.g. token unavailable) — identity check skipped, size
	// guard still applies.
	if err := checkAgentHello("", 80*gib, 0, "/dev/sda", mk("default", 80*gib)); err != nil {
		t.Errorf("empty expected job must skip the identity check: %v", err)
	}
	if err := checkAgentHello("", 80*gib, 0, "/dev/sda", mk("default", 512<<20)); err == nil {
		t.Error("size guard must still apply when the identity check is skipped")
	}
}

// enrollJobID must be stable and match the manifest scoping (16-char token
// prefix) — the installer bakes it into the agent's -job flag and the receiver
// compares it on every session.
func TestEnrollJobID(t *testing.T) {
	if got := enrollJobID("0123456789abcdefEXTRA"); got != "0123456789abcdef" {
		t.Errorf("enrollJobID long token = %q, want first 16 chars", got)
	}
	if got := enrollJobID("short"); got != "short" {
		t.Errorf("enrollJobID short token = %q, want unchanged", got)
	}
}
