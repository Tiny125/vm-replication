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

// F-31: the console used to tell the operator the boot image was "VALIDATED
// as bootable" — on the strength of one grep over a GRUB config the
// converter had just written itself, which is not evidence of an actual
// boot. A real AWS EC2 -> Linode migration reported exactly that and
// produced a machine that sat at a grub> prompt. The dialog and the
// awaiting-cutover banner must say what was actually checked (the GRUB
// configuration, not a live boot) and must not claim a boot was validated.
func TestConsoleBlockCutoverChecksConfigNotBoot(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	if !strings.Contains(js, "check its GRUB configuration") {
		t.Error("cutover dialog should say step 1 converts the boot image and checks its GRUB configuration, not that it validates a boot")
	}
	if strings.Contains(js, "validate it is bootable") {
		t.Error(`cutover dialog must not say "validate it is bootable" (F-31: that is not what the check proves)`)
	}
	if !strings.Contains(consoleHTML, "GRUB configuration checked for errors") {
		t.Error("awaiting-cutover banner should say the GRUB configuration was checked, not that the image was validated bootable")
	}
	if strings.Contains(consoleHTML, "converted and validated as bootable") {
		t.Error(`awaiting-cutover banner must not say "converted and validated as bootable" (F-31)`)
	}
}
