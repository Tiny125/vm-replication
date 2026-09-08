package appliance

import (
	"strings"
	"testing"
)

// decideBootTarget (F-31) replaces decideBootKernel. F-19 established that
// "vmrepl-layout: wholedisk" alone does not mean "no bootloader" (Linode's
// own partitionless images boot fine via linode/grub2). F-31, found on a real
// AWS EC2 -> Linode migration, is the mirror-image trap: a PARTITIONED image
// with a perfectly good grub.cfg still does NOT boot via linode/grub2,
// because Linode's host-side GRUB never reads a config off a partition —
// proven live at the grub> prompt:
//
//	grub> ls (hd0,gpt16)/grub/grub.cfg      -> grub.cfg      (present, correct)
//	grub> [drops straight to the shell; the config is never loaded]
//
// linode/direct-disk boots the SAME image unattended
// (09:07:58 ping=ICMP-REPLY ssh=tcp22-OPEN) but ONLY with root_device=/dev/sda
// (the WHOLE disk) — root_device=/dev/sda1 FAILS the boot outright
// (linode_reboot status=failed, no message). That is the trap this table
// specifically guards against: a partitioned root's own vmrepl-root value
// (e.g. /dev/sda1) must NEVER leak into direct-disk's root_device.
func TestDecideBootTarget(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantKernel string
		wantRoot   string
		wantWarn   bool
	}{
		{
			// Case 1: today's only proven-good case before F-31 — Linode's own
			// partitionless images.
			name:       "wholedisk + grub2 -> linode/grub2, root /dev/sda",
			out:        "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-bootloader: grub2\nvmrepl-mbrboot: absent\nvmrepl-root: /dev/sda\n",
			wantKernel: "linode/grub2",
			wantRoot:   "/dev/sda",
		},
		{
			// Case 2: the F-31 fix itself, verified live against the AWS AMI.
			// vmrepl-root reports /dev/sda16 (the real root partition) — the
			// ladder must ignore it and use the WHOLE disk for direct-disk.
			name:       "partitioned + mbrboot present -> linode/direct-disk, root /dev/sda (never the partition)",
			out:        "Root filesystem: /dev/sda16 (partitioned=1)\nvmrepl-layout: partitioned\nvmrepl-bootloader: grub2\nvmrepl-mbrboot: present\nvmrepl-root: /dev/sda16\n",
			wantKernel: "linode/direct-disk",
			wantRoot:   "/dev/sda",
		},
		{
			name:       "bootloader none -> linode/latest-64bit, root = vmrepl-root",
			out:        "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-bootloader: none\nvmrepl-mbrboot: absent\nvmrepl-root: /dev/sda\n",
			wantKernel: "linode/latest-64bit",
			wantRoot:   "/dev/sda",
		},
		{
			// Case 4: UEFI-only / non-BIOS-GRUB source — the case that runs
			// Linode's kernel instead of the distro's and must warn loudly
			// (this silently disabled SELinux on a real CentOS Stream 9 box, see
			// decideBootTarget's comment).
			name:       "partitioned + mbrboot absent -> linode/latest-64bit, root = vmrepl-root, LOUD warn",
			out:        "Root filesystem: /dev/sda1 (partitioned=1)\nvmrepl-layout: partitioned\nvmrepl-bootloader: grub2\nvmrepl-mbrboot: absent\nvmrepl-root: /dev/sda1\n",
			wantKernel: "linode/latest-64bit",
			wantRoot:   "/dev/sda1",
			wantWarn:   true,
		},
		{
			// Legacy fallback: a convert script from before the vmrepl-bootloader
			// marker existed emits vmrepl-layout alone. Must regress to the old
			// (safe-if-suboptimal) F-19 rule, not to something new/unvalidated.
			name:       "no markers at all + wholedisk text -> legacy fallback (latest-64bit)",
			out:        "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-root: /dev/sda\n",
			wantKernel: "linode/latest-64bit",
			wantRoot:   "/dev/sda",
		},
		{
			name:       "no markers at all + partitioned text -> legacy fallback (grub2)",
			out:        "Root filesystem: /dev/sdc1 (partitioned=1)\nvmrepl-layout: partitioned\nvmrepl-root: /dev/sda1\n",
			wantKernel: "linode/grub2",
			wantRoot:   "/dev/sda",
		},
		{
			name:       "empty output falls back to the default GRUB2 path",
			out:        "",
			wantKernel: "linode/grub2",
			wantRoot:   "/dev/sda",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideBootTarget(c.out)
			if got.kernel != c.wantKernel {
				t.Errorf("decideBootTarget(%q).kernel = %q, want %q", c.out, got.kernel, c.wantKernel)
			}
			if got.rootDevice != c.wantRoot {
				t.Errorf("decideBootTarget(%q).rootDevice = %q, want %q", c.out, got.rootDevice, c.wantRoot)
			}
			if c.wantWarn && got.warn == "" {
				t.Errorf("decideBootTarget(%q): expected a loud warning naming the consequence, got none", c.out)
			}
			if !c.wantWarn && got.warn != "" {
				t.Errorf("decideBootTarget(%q): unexpected warning: %q", c.out, got.warn)
			}
		})
	}
}

// The direct-disk trap, stated as its own assertion: root_device=/dev/sda1
// FAILED the live boot outright while /dev/sda booted unattended (F-31). No
// case in the ladder may ever pair linode/direct-disk with anything other
// than the whole disk.
func TestDecideBootTargetDirectDiskNeverPartitioned(t *testing.T) {
	cases := []string{
		"vmrepl-layout: partitioned\nvmrepl-bootloader: grub2\nvmrepl-mbrboot: present\nvmrepl-root: /dev/sda1\n",
		"vmrepl-layout: partitioned\nvmrepl-bootloader: grub2\nvmrepl-mbrboot: present\nvmrepl-root: /dev/sda16\n",
		"vmrepl-layout: partitioned\nvmrepl-bootloader: none\nvmrepl-mbrboot: present\nvmrepl-root: /dev/sda2\n",
	}
	for _, out := range cases {
		got := decideBootTarget(out)
		if got.kernel == "linode/direct-disk" && got.rootDevice != "/dev/sda" {
			t.Errorf("decideBootTarget(%q) chose direct-disk with root %q, want /dev/sda (partition boot FAILS live: linode_reboot status=failed)", out, got.rootDevice)
		}
	}
}

// convertFailureNoRoot must recognise the convert script's "no root filesystem"
// failure (wrong source device — e.g. a swap disk) so the cutover aborts with the
// right guidance instead of launching an unbootable grub> instance.
func TestConvertFailureNoRoot(t *testing.T) {
	noRoot := "Re-reading partition table on /dev/sdc\nno automatic fs check for /dev/sdc (swap); skipping\n" +
		"could not locate a root filesystem with /etc/fstab on /dev/sdc (candidates: /dev/sdc)\n" +
		"NAME     TYPE FSTYPE SIZE\n/dev/sdc disk swap    80G"
	if !convertFailureNoRoot(noRoot) {
		t.Error("expected no-root failure to be detected")
	}
	for _, out := range []string{
		"Root filesystem: /dev/sdc1 (partitioned=1)\nboot disk converted for Linode",
		"e2fsck: Bad magic number in super-block",
		"",
	} {
		if convertFailureNoRoot(out) {
			t.Errorf("did not expect no-root for: %q", out)
		}
	}
}

// convertFailureMountIssue must recognise the chroot-setup mount failure (a
// pseudo-fs mount point that is not a directory) so the cutover reports a
// conversion-environment problem (update + retry) instead of blaming an
// inconsistent source (re-sync). This is the failure from migration #2's log:
// fsck passed, then the /proc mount aborted the run.
func TestConvertFailureMountIssue(t *testing.T) {
	mountFail := ">> Root filesystem: /dev/sdd (partitioned=0)\nvmrepl-layout: wholedisk\n" +
		">> Mounting root and binding kernel filesystems\n" +
		"mount: /tmp/tmp.WOvynQBXdI/proc: mount point is not a directory.\n" +
		"       dmesg(1) may have more information after failed mount system call."
	if !convertFailureMountIssue(mountFail) {
		t.Error("expected mount-point failure to be detected")
	}
	// A mount failure is an environment issue, not a wrong-disk / no-root one.
	if convertFailureNoRoot(mountFail) {
		t.Error("mount failure must not be classified as no-root")
	}
	for _, out := range []string{
		"boot disk converted for Linode (virtio, network)",
		"could not locate a root filesystem with /etc/fstab on /dev/sdc",
		"",
	} {
		if convertFailureMountIssue(out) {
			t.Errorf("did not expect mount-issue for: %q", out)
		}
	}
}

// F-31 (commit 2): the product used to claim "VALIDATED as bootable" on the
// strength of one grep over the config the converter itself just wrote — and
// that check was SKIPPED ENTIRELY (while the script still exited 0) whenever
// no grub-mkconfig/update-grub tool existed in the chroot. bootcfgNote must
// state plainly what was actually checked for every vmrepl-bootcfg value,
// and must never claim a real boot was validated.
func TestBootcfgNote(t *testing.T) {
	cases := []struct {
		status       string
		mustContain  string
		mustNotClaim bool // "validated as bootable" / "VALIDATED" must not appear
	}{
		{status: "ok", mustContain: "checked", mustNotClaim: true},
		{status: "skipped-notool", mustContain: "NOT", mustNotClaim: true},
		{status: "skipped-nobootloader", mustContain: "no GRUB", mustNotClaim: true},
		{status: "", mustContain: "could not be checked", mustNotClaim: true},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := bootcfgNote(c.status)
			if got == "" {
				t.Fatalf("bootcfgNote(%q) returned empty", c.status)
			}
			if !strings.Contains(got, c.mustContain) {
				t.Errorf("bootcfgNote(%q) = %q, want it to contain %q", c.status, got, c.mustContain)
			}
			if c.mustNotClaim && strings.Contains(strings.ToLower(got), "validated as bootable") {
				t.Errorf("bootcfgNote(%q) = %q must not claim a real boot was validated", c.status, got)
			}
		})
	}
	// The skipped-notool note in particular must read as UNVALIDATED, not as
	// a pass — this is the exact path that used to exit 0 with no signal.
	if !strings.Contains(bootcfgNote("skipped-notool"), "UNVALIDATED") {
		t.Error(`bootcfgNote("skipped-notool") must say the config is UNVALIDATED, not silently pass`)
	}
}

// cutoverStep1DoneMsg is the guided-cutover "step 1 done" activity-log line
// shown to the operator right before they are told it is safe to power off
// the source (migrations.go, formerly "the boot image was converted and
// VALIDATED as bootable"). F-31: that phrase is exactly what sent a real
// operator to decommission a source whose migrated machine never booted. The
// new message must never claim a real boot was validated, and must say
// clearly that the guest boot is checked separately (see verifyGuestBoot).
func TestCutoverStep1DoneMsg(t *testing.T) {
	for _, status := range []string{"ok", "skipped-notool", "skipped-nobootloader", ""} {
		msg := cutoverStep1DoneMsg(status)
		if strings.Contains(msg, "VALIDATED as bootable") {
			t.Errorf("cutoverStep1DoneMsg(%q) = %q must not claim VALIDATED as bootable (F-31)", status, msg)
		}
		if !strings.Contains(msg, "does NOT prove the guest will boot") {
			t.Errorf("cutoverStep1DoneMsg(%q) = %q must say this does not prove the guest boots", status, msg)
		}
	}
}
