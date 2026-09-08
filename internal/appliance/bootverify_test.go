package appliance

import (
	"context"
	"strings"
	"testing"
	"time"
)

// F-31: WaitInstanceStatus(..., "running", ...) is not evidence the GUEST
// booted — confirmed against the live API, a Linode sitting at a grub>
// prompt reports "running" the whole time it sits there. A guest at a grub>
// prompt has NO IP stack: no ICMP reply, and no TCP response at all, not
// even a RST. So an ICMP echo reply, OR an open TCP port, OR a definite
// "connection refused" (reachable) is positive proof the guest kernel
// booted and configured its network. Anything else (timeouts only) is NOT
// proof of failure — a booted guest behind a deny-all firewall gives no
// signal either — and must never be reported as, or read as, success.
//
// fakePing/fakeDial let the table below drive verifyGuestBootWith without
// touching a real network.
func fakePing(ok bool, detail string) pingFunc {
	return func(ctx context.Context, host string) (bool, string) { return ok, detail }
}

func fakeDial(open, refused bool, detail string) portDialFunc {
	return func(ctx context.Context, host string, port int, timeout time.Duration) (bool, bool, string) {
		return open, refused, detail
	}
}

func TestVerifyGuestBoot(t *testing.T) {
	cases := []struct {
		name         string
		ping         pingFunc
		dial         portDialFunc
		wantVerified bool
		wantContains string
	}{
		{
			name:         "ICMP echo reply -> verified",
			ping:         fakePing(true, "reachable"),
			dial:         fakeDial(false, false, "timed out"),
			wantVerified: true,
			wantContains: "ICMP",
		},
		{
			name:         "TCP refused (reachable, nothing listening) -> verified",
			ping:         fakePing(false, "no ICMP reply"),
			dial:         fakeDial(false, true, "connection refused (reachable, nothing listening)"),
			wantVerified: true,
			wantContains: "refused",
		},
		{
			name:         "TCP open -> verified",
			ping:         fakePing(false, "no ICMP reply"),
			dial:         fakeDial(true, false, "TCP connect succeeded"),
			wantVerified: true,
			wantContains: "open",
		},
		{
			name:         "all timeouts (no ping, no dial signal at all) -> NOT verified",
			ping:         fakePing(false, "no ICMP reply within timeout"),
			dial:         fakeDial(false, false, "timed out (filtered by a firewall / security group, or host down)"),
			wantVerified: false,
			wantContains: "NOT proof",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Tiny budget/poll so the "all timeouts" case doesn't actually wait
			// minutes in the test suite.
			verified, evidence := verifyGuestBootWith(context.Background(), "192.0.2.1", 30*time.Millisecond, 5*time.Millisecond, c.ping, c.dial)
			if verified != c.wantVerified {
				t.Errorf("verified = %v, want %v (evidence: %q)", verified, c.wantVerified, evidence)
			}
			if !strings.Contains(evidence, c.wantContains) {
				t.Errorf("evidence = %q, want it to contain %q", evidence, c.wantContains)
			}
		})
	}
}

// A canceled context must stop the poll promptly and report unverified, not
// hang until the budget expires.
func TestVerifyGuestBootContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	verified, evidence := verifyGuestBootWith(ctx, "192.0.2.1", time.Minute, time.Second,
		fakePing(false, "no reply"), fakeDial(false, false, "timed out"))
	if verified {
		t.Error("a canceled context must not report verified")
	}
	if evidence == "" {
		t.Error("expected a non-empty explanation even when canceled")
	}
}

// F-31: the two launch-event builders must NEVER let the phrase "migration
// complete" appear in the UNVERIFIED branch — that is the exact phrase a
// real operator saw right before decommissioning a source whose migrated
// machine never booted. The VERIFIED branch is the only one allowed to say
// it.
func TestLaunchEventTextNeverClaimsCompleteWhenUnverified(t *testing.T) {
	t.Run("volume boot", func(t *testing.T) {
		lvl, verifiedText := volumeBootLaunchEvent("my-mig-cutover", 12345, 2, true, "answered ICMP at 192.0.2.1")
		if lvl != "info" {
			t.Errorf("verified level = %q, want info", lvl)
		}
		if !strings.Contains(verifiedText, "migration complete") {
			t.Errorf("verified text must say migration complete: %q", verifiedText)
		}

		lvl, unverifiedText := volumeBootLaunchEvent("my-mig-cutover", 12345, 2, false, "no ICMP reply and no TCP response within 5m")
		if lvl != "warn" {
			t.Errorf("unverified level = %q, want warn", lvl)
		}
		if strings.Contains(unverifiedText, "migration complete") {
			t.Errorf("unverified text must NOT contain %q: %q", "migration complete", unverifiedText)
		}
		if !strings.Contains(unverifiedText, "Lish") {
			t.Errorf("unverified text should point the operator at the Lish console: %q", unverifiedText)
		}
		if !strings.Contains(unverifiedText, "NOT proof") {
			t.Errorf("unverified text should say this is not proof of failure: %q", unverifiedText)
		}
	})
	t.Run("disk boot", func(t *testing.T) {
		lvl, verifiedText := diskBootLaunchEvent("my-mig-cutover", 12345, "g6-standard-2", "", true, "TCP 22 refused at 192.0.2.1")
		if lvl != "info" {
			t.Errorf("verified level = %q, want info", lvl)
		}
		if !strings.Contains(verifiedText, "migration complete") {
			t.Errorf("verified text must say migration complete: %q", verifiedText)
		}

		lvl, unverifiedText := diskBootLaunchEvent("my-mig-cutover", 12345, "g6-standard-2", "", false, "no ICMP reply and no TCP response within 5m")
		if lvl != "warn" {
			t.Errorf("unverified level = %q, want warn", lvl)
		}
		if strings.Contains(unverifiedText, "migration complete") {
			t.Errorf("unverified text must NOT contain %q: %q", "migration complete", unverifiedText)
		}
		if !strings.Contains(unverifiedText, "Lish") {
			t.Errorf("unverified text should point the operator at the Lish console: %q", unverifiedText)
		}
	})
}

// The console banner must be DERIVED from BootVerified/BootEvidence (no new
// migration state), and must not present an unverified boot as an
// unqualified "Migration complete".
func TestConsoleDerivesBootVerificationBanner(t *testing.T) {
	js := extractJSFunc(t, "function migCard(")
	if !strings.Contains(js, "v.boot_verified") || !strings.Contains(js, "v.boot_evidence") {
		t.Error("migCard must derive its banner from v.boot_verified/v.boot_evidence, not a new migration state")
	}
	if !strings.Contains(js, "guest boot NOT verified") {
		t.Error("migCard must show a distinct banner when the launched guest's boot could not be confirmed")
	}
}
