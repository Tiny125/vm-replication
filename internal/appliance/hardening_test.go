package appliance

import (
	"strings"
	"testing"
)

// diskFitError is the disk-boot fit gate: cloud images are PARTITIONED, so the
// whole-disk ext shrink is skipped and (before this) nothing checked whether
// the unshrunk image fits the plan's actual local disk — the rescue copy then
// failed at the very end (dd: no space) and the cutover timed out instead of
// failing fast with advice.
func TestDiskFitError(t *testing.T) {
	const mib = int64(1) << 20
	cases := []struct {
		name       string
		sizeBytes  int64
		shrunkMB   int
		actualMB   int
		wantErrSub string // "" = must fit
	}{
		{"shrunk image fits", 81920 * mib, 81904, 81920, ""},
		{"shrunk image too big", 81920 * mib, 90000, 81920, "larger plan"},
		{"unshrunk image fits smaller disk", 30720 * mib, 0, 81920, ""},
		{"unshrunk image exactly fits", 81920 * mib, 0, 81920, ""},
		{"unshrunk image too big (partitioned cloud image)", 90000 * mib, 0, 81920, "cannot be shrunk"},
		{"unknown disk size never blocks", 90000 * mib, 0, 0, ""},
	}
	for _, c := range cases {
		got := diskFitError(c.sizeBytes, c.shrunkMB, c.actualMB, "g6-standard-2")
		if c.wantErrSub == "" && got != "" {
			t.Errorf("%s: unexpected fit error %q", c.name, got)
		}
		if c.wantErrSub != "" && !strings.Contains(got, c.wantErrSub) {
			t.Errorf("%s: fit error %q should mention %q", c.name, got, c.wantErrSub)
		}
	}
}

// The enrollment installer must refuse non-x86_64 sources (EC2 Graviton, GCP
// T2A, Azure Ampere): the agent binary is linux/amd64 and Linode compute is
// x86_64-only — without the guard the install fails with a cryptic
// "exec format error" and a migrated ARM image could never boot anyway.
func TestInstallerRejectsNonX86(t *testing.T) {
	for _, want := range []string{"uname -m", "x86_64"} {
		if !strings.Contains(agentInstallerScript, want) {
			t.Errorf("installer script must carry the architecture guard (missing %q)", want)
		}
	}
	// The guard must run BEFORE the agent binary download.
	guard := strings.Index(agentInstallerScript, "uname -m")
	download := strings.Index(agentInstallerScript, "Downloading agent")
	if guard < 0 || download < 0 || guard > download {
		t.Error("the architecture guard must run before the agent download")
	}
}
