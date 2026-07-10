package appliance

import (
	"strings"
	"testing"
)

// A guided block cutover converts the boot disk in phase 1 (before power-off) and
// caches the result for phase 2. The cache must round-trip and be consumed once,
// so phase 2 reuses phase 1's conversion instead of re-running it — and a retry
// (or a restart that clears the cache) converts fresh.
func TestCutoverConvertCache(t *testing.T) {
	s := &Server{}
	if _, ok := s.takeCutoverConvert(1); ok {
		t.Error("no cached result should exist initially")
	}
	s.storeCutoverConvert(1, convertOutcome{kernel: "linode/latest-64bit", rootDevice: "/dev/sda1"})
	co, ok := s.takeCutoverConvert(1)
	if !ok || co.kernel != "linode/latest-64bit" || co.rootDevice != "/dev/sda1" {
		t.Fatalf("cached convert result not returned: ok=%v co=%+v", ok, co)
	}
	if _, ok := s.takeCutoverConvert(1); ok {
		t.Error("cached result must be consumed once (deleted on take)")
	}
}

// Once cutover has been reached, the PRE-migration validation checks (agent
// connected, replication lag) are only informational — the source agent is
// stopped — so the card must render them as a muted "not applicable" marker
// rather than an alarming red ✗ that reads as an outstanding error.
func TestConsoleNeutralizesPostCutoverChecks(t *testing.T) {
	js := extractJSFunc(t, "function migCard(")
	if !strings.Contains(js, "postCutover") {
		t.Error("card must compute a post-cutover flag to neutralize informational checks")
	}
	// Post-cutover pre-checks render muted with a clarifying note instead of ✗.
	if !strings.Contains(js, "not applicable after cutover") {
		t.Error("post-cutover pre-migration checks should be labelled not-applicable, not shown as errors")
	}
	// The neutralized marker is muted, not the red ✗ class.
	if !strings.Contains(js, `info?'muted">`) {
		t.Error("an informational failed check must use the muted marker, not the red ✗")
	}
}

// The block cutover flow must tell the operator, in the dialog and on the card,
// that the boot image is converted and VALIDATED before they power off the source
// — the whole point of moving the conversion into phase 1.
func TestConsoleBlockCutoverValidatesBeforePowerOff(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	if !strings.Contains(js, "convert the boot image and validate") {
		t.Error("cutover dialog should say step 1 converts + validates the boot image before power-off")
	}
	if !strings.Contains(consoleHTML, "converted and validated as bootable") {
		t.Error("awaiting-cutover banner should confirm the image was validated before power-off")
	}
}
