package main

import "testing"

// The file walk must never copy the destination's own boot/kernel/network
// plumbing, virtual filesystems, or the agent's own install — these are
// excluded at the source so the destination keeps booting on its native OS.
// (The receiver has an isProtectedDestPath backstop for the same set.)
func TestExcludedFromFileCopy(t *testing.T) {
	excluded := []string{
		"proc", "proc/1/stat", "sys/kernel", "dev/null", "run/lock", "tmp/x",
		"boot", "boot/grub/grub.cfg", "vmlinuz", "lib/modules/6.1.0/x.ko",
		"etc/fstab", "etc/machine-id", "etc/resolv.conf",
		"etc/netplan/01.yaml", "etc/systemd/network/10.network",
		"etc/NetworkManager/system-connections/x", "etc/network/interfaces",
		"usr/local/bin/vmrepl-agent", "etc/vm-repl/agent.crt",
		"var/lib/vmrepl-source-abc-disk0.cbt", "var/tmp/junk", "mnt/data", "lost+found",
	}
	for _, p := range excluded {
		if !excludedFromFileCopy(p) {
			t.Errorf("%q must be excluded from the file copy", p)
		}
	}
	kept := []string{
		"etc/hostname", "etc/ssh/sshd_config", "home/user/.bashrc",
		"var/www/index.html", "opt/app/bin/server", "usr/bin/python3",
		"etc/systemd/system/app.service", // unit files ARE copied (not etc/systemd/network)
		"srv/data.db", "root/.ssh/authorized_keys",
	}
	for _, p := range kept {
		if excludedFromFileCopy(p) {
			t.Errorf("%q must be copied (not excluded)", p)
		}
	}
}
