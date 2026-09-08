package appliance

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// ---------------------------------------------------------------------------
// F-31: verify the guest actually booted.
//
// WaitInstanceStatus(ctx, inst.ID, "running", ...) is not evidence of a guest
// boot — confirmed against the live Linode API: the broken aws-instance-test
// instance reported "running" the whole time it sat at a `grub>` prompt.
//
// A guest at a `grub>` prompt has NO IP stack: no ICMP reply, and no TCP
// response at all — not even a RST (connection refused). So any of the
// following is positive proof the guest kernel booted and configured its
// network:
//   - an ICMP echo reply
//   - an open TCP port
//   - a definite "connection refused" on a probed port (the OS answered —
//     reachable, just nothing listening there)
//
// The absence of all three (timeouts only) is NOT proof of failure — a
// booted guest sitting behind a deny-all firewall gives exactly the same
// signal — and must never be reported as, or read as, success.
// ---------------------------------------------------------------------------

// pingFunc/portDialFunc match pingHost's and dialPort's signatures exactly
// (diagnostics.go), so production code passes them directly and tests inject
// fakes without touching a real network.
type pingFunc func(ctx context.Context, host string) (bool, string)
type portDialFunc func(ctx context.Context, host string, port int, timeout time.Duration) (open, refused bool, detail string)

// guestBootPorts are probed in order for each poll: SSH first (almost always
// present), then the two common web ports.
var guestBootPorts = []int{22, 80, 443}

// verifyGuestBootWith polls for evidence the guest actually booted, up to
// budget, sleeping poll between attempts. ping/dial are injected so tests can
// drive it without a real network; verifyGuestBoot (below) wires up the real
// pingHost/dialPort for production use.
func verifyGuestBootWith(ctx context.Context, host string, budget, poll time.Duration, ping pingFunc, dial portDialFunc) (verified bool, evidence string) {
	deadline := time.Now().Add(budget)
	for {
		if ok, detail := ping(ctx, host); ok {
			return true, fmt.Sprintf("answered ICMP at %s (%s)", host, detail)
		}
		for _, p := range guestBootPorts {
			open, refused, detail := dial(ctx, host, p, 5*time.Second)
			if open {
				return true, fmt.Sprintf("TCP %d open at %s (%s)", p, host, detail)
			}
			if refused {
				return true, fmt.Sprintf("TCP %d refused (reachable) at %s (%s)", p, host, detail)
			}
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false, fmt.Sprintf(
				"no ICMP reply and no TCP response (open or refused) on ports %v at %s within %s. "+
					"This is NOT proof the boot failed — a booted guest behind a deny-all firewall gives no "+
					"signal either — but it is also not proof of success. Open the instance's Lish console "+
					"and confirm it reaches a login prompt before you decommission the source.",
				guestBootPorts, host, budget.Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return false, "boot verification canceled before any signal was seen"
		case <-time.After(poll):
		}
	}
}

// verifyGuestBoot is the production entry point: ~5 minutes budget, polling
// every ~15s, using the real pingHost/dialPort.
func verifyGuestBoot(ctx context.Context, host string) (bool, string) {
	return verifyGuestBootWith(ctx, host, 5*time.Minute, 15*time.Second, pingHost, dialPort)
}

// ---------------------------------------------------------------------------
// Pure event-text builders. F-31: the OLD "migration complete" event fired
// unconditionally the moment Boot() succeeded — before this fix, nothing
// ever checked whether the guest actually came up. These two are kept as
// small pure functions (no side effects) so the wording is unit-tested
// directly: in particular, the phrase "migration complete" must appear ONLY
// in the verified branch. That is exactly the phrase a real operator saw
// right before decommissioning a source whose migrated machine never
// booted.
// ---------------------------------------------------------------------------

const unverifiedBootNote = "This is NOT proof of failure — a booted guest behind a deny-all firewall gives no signal either — but it is also not proof of success. Open the instance's Lish console and confirm it reaches a login prompt BEFORE you decommission the source."

// volumeBootLaunchEvent builds the activity-log level+text for a completed
// volume-boot cutover launch.
func volumeBootLaunchEvent(label string, instID int64, nVolumes int, verified bool, evidence string) (level, text string) {
	if verified {
		return "info", fmt.Sprintf("migration complete: launched cutover Linode %q (id %d) from %d volume(s) — guest boot verified (%s)", label, instID, nVolumes, evidence)
	}
	return "warn", fmt.Sprintf("launched cutover Linode %q (id %d) from %d volume(s), but the guest's boot could NOT be confirmed (%s). %s", label, instID, nVolumes, evidence, unverifiedBootNote)
}

// diskBootLaunchEvent builds the activity-log level+text for a completed
// disk-boot (local-disk) cutover launch.
func diskBootLaunchEvent(instLabel string, instID int64, plan, withData string, verified bool, evidence string) (level, text string) {
	if verified {
		return "info", fmt.Sprintf("migration complete: %q (id %d) is booting from its local disk on plan %s%s — guest boot verified (%s)", instLabel, instID, plan, withData, evidence)
	}
	return "warn", fmt.Sprintf("%q (id %d) is running on plan %s%s, but the guest's boot could NOT be confirmed (%s). %s", instLabel, instID, plan, withData, evidence, unverifiedBootNote)
}

// ---------------------------------------------------------------------------
// Persistence: BootVerified/BootEvidence are NOT a new migration state (that
// would ripple through stateClass/stateLabel/STATE_DESCS/pillFor/the store
// enum and many tests). They are persisted per-migration with the existing
// SetSetting/GetSetting pattern — no schema migration — exactly like
// cutoverStreamSettingKey (cutover_stream.go).
// ---------------------------------------------------------------------------

// bootVerifyResult is the durable, JSON-encoded record of one migration's
// guest-boot verification outcome.
type bootVerifyResult struct {
	Verified bool   `json:"verified"`
	Evidence string `json:"evidence"`
}

// bootVerifySettingKey is the settings-table key holding one migration's
// persisted boot-verification result.
func bootVerifySettingKey(migID int64) string {
	return fmt.Sprintf("boot_verify:%d", migID)
}

// saveBootVerify persists a migration's guest-boot verification outcome. A
// nil store (bare Server{} used by unit tests) is a deliberate no-op.
func (s *Server) saveBootVerify(migID int64, verified bool, evidence string) {
	if s.st == nil {
		return
	}
	b, err := json.Marshal(bootVerifyResult{Verified: verified, Evidence: evidence})
	if err != nil {
		log.Printf("appliance: persist boot verification for migration %d: %v", migID, err)
		return
	}
	if err := s.st.SetSetting(s.ctx, bootVerifySettingKey(migID), string(b)); err != nil {
		log.Printf("appliance: persist boot verification for migration %d: %v", migID, err)
	}
}

// loadBootVerify reads back a migration's persisted boot-verification
// result, if any (view() uses this to derive the console banner).
func (s *Server) loadBootVerify(ctx context.Context, migID int64) (bootVerifyResult, bool) {
	if s.st == nil {
		return bootVerifyResult{}, false
	}
	v, ok, err := s.st.GetSetting(ctx, bootVerifySettingKey(migID))
	if err != nil || !ok {
		return bootVerifyResult{}, false
	}
	var rec bootVerifyResult
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		log.Printf("appliance: decode persisted boot verification for migration %d: %v", migID, err)
		return bootVerifyResult{}, false
	}
	return rec, true
}
