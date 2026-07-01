package appliance

import "testing"

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
