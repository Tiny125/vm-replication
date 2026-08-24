package appliance

import "testing"

// decideBootKernel (F-19): the boot kernel choice must come from whether the
// image actually carries a usable bootloader (the vmrepl-bootloader marker),
// NOT from partitioning. Linode's own CentOS Stream 9 / Ubuntu 24.04 images
// are partitionless (vmrepl-layout: wholedisk) yet still boot via
// linode/grub2 — a migrated CentOS Stream 9 instance was wrongly booted with
// linode/latest-64bit (the Linode kernel) purely because its root happened to
// be a partitionless filesystem, which silently disabled SELinux even though
// /etc/selinux/config still said "enforcing".
func TestDecideBootKernel(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "bootloader marker grub2 wins even when wholedisk",
			out:  "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-bootloader: grub2\nvmrepl-root: /dev/sda\n",
			want: "linode/grub2",
		},
		{
			name: "bootloader marker none forces the Linode kernel",
			out:  "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-bootloader: none\nvmrepl-root: /dev/sda\n",
			want: "linode/latest-64bit",
		},
		{
			name: "bootloader marker absent + wholedisk falls back to the OLD behaviour (back-compat with a pre-F-19 convert script)",
			out:  "Root filesystem: /dev/sdc (partitioned=0)\nvmrepl-layout: wholedisk\nvmrepl-root: /dev/sda\n",
			want: "linode/latest-64bit",
		},
		{
			name: "bootloader marker absent + partitioned keeps the default GRUB2 path",
			out:  "Root filesystem: /dev/sdc1 (partitioned=1)\nvmrepl-layout: partitioned\nvmrepl-root: /dev/sda1\n",
			want: "linode/grub2",
		},
		{
			name: "partitioned with an explicit grub2 marker",
			out:  "Root filesystem: /dev/sdc1 (partitioned=1)\nvmrepl-layout: partitioned\nvmrepl-bootloader: grub2\nvmrepl-root: /dev/sda1\n",
			want: "linode/grub2",
		},
		{
			name: "empty output falls back to the default GRUB2 path",
			out:  "",
			want: "linode/grub2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideBootKernel(c.out); got != c.want {
				t.Errorf("decideBootKernel(%q) = %q, want %q", c.out, got, c.want)
			}
		})
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
