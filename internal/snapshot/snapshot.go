// Package snapshot establishes a consistent point-in-time view of a source
// device before the agent reads it, so the replicated image is consistent
// rather than merely crash-consistent.
//
// Modes:
//
//   - none:     read the live device directly (crash-consistent).
//   - fsfreeze: FIFREEZE the filesystem for the duration of the read. Correct
//     but holds writes frozen the whole time — use only for a short
//     final cutover pass, not continuous replication.
//   - lvm:      take an LVM copy-on-write snapshot of the origin LV and read
//     from that. The source keeps running while we replicate from a
//     stable snapshot. This is the recommended app-consistent mode.
//
// Application consistency comes from the optional pre/post hooks: run a pre-hook
// to quiesce the app (e.g. `mysql -e 'FLUSH TABLES WITH READ LOCK'` or
// `fsfreeze`-aware DB flush), the snapshot is taken at that instant, then the
// post-hook resumes the app — so the snapshot reflects a clean app state.
//
// These modes require root and the relevant tooling (util-linux fsfreeze, LVM2)
// and operate on real devices, so they cannot run in a sandbox; "none" is the
// default and keeps the tool fully usable without them.
package snapshot

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// Mode selects the consistency strategy.
type Mode string

const (
	ModeNone     Mode = "none"
	ModeFsfreeze Mode = "fsfreeze"
	ModeLVM      Mode = "lvm"
)

// Options configures Prepare.
type Options struct {
	Mode        Mode
	Device      string // source device/LV to replicate (for lvm: the origin LV)
	Mountpoint  string // filesystem mount to freeze; auto-detected if empty
	PreHook     string // shell command to quiesce the app before the snapshot
	PostHook    string // shell command to resume the app after the snapshot
	LVMSnapSize string // CoW size for the LVM snapshot, e.g. "5G"
	SnapName    string // LVM snapshot name; defaults to vmrepl-snap-<ts>
}

// DetectMode picks the best zero-input consistency strategy for device: an LVM
// copy-on-write snapshot when device is an LVM logical volume (the source keeps
// running, no downtime), otherwise fsfreeze (briefly pauses writes while the
// point-in-time read runs). Both yield a single crash-consistent instant; LVM is
// strongly preferred because it is non-disruptive. Used at cutover when the
// operator hasn't pinned a -snapshot mode.
func DetectMode(device string) Mode {
	if isLVM(device) {
		return ModeLVM
	}
	return ModeFsfreeze
}

// isLVM reports whether device is an LVM logical volume (so an LVM snapshot is
// possible). It is best-effort: if lvs is absent or errors, we treat the device
// as non-LVM and fall back to fsfreeze.
func isLVM(device string) bool {
	if _, err := exec.LookPath("lvs"); err != nil {
		return false
	}
	out, err := run("lvs", "--noheadings", "-o", "lv_name", device)
	return err == nil && strings.TrimSpace(out) != ""
}

// Prepare sets up the consistency point and returns the path to read from plus
// a cleanup function that the caller must always invoke (e.g. via defer).
func Prepare(o Options) (readPath string, cleanup func(), err error) {
	switch o.Mode {
	case "", ModeNone:
		return o.Device, func() {}, nil
	case ModeFsfreeze:
		return prepareFsfreeze(o)
	case ModeLVM:
		return prepareLVM(o)
	default:
		return "", func() {}, fmt.Errorf("snapshot: unknown mode %q", o.Mode)
	}
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func runShell(cmd string) error {
	if cmd == "" {
		return nil
	}
	log.Printf("snapshot: running hook: %s", cmd)
	if out, err := run("/bin/sh", "-c", cmd); err != nil {
		return fmt.Errorf("hook failed: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return nil
}

// mountpointFor returns the mountpoint of dev, or o.Mountpoint if set.
func mountpointFor(o Options) (string, error) {
	if o.Mountpoint != "" {
		return o.Mountpoint, nil
	}
	out, err := run("findmnt", "-n", "-o", "TARGET", "--source", o.Device)
	if err != nil {
		return "", fmt.Errorf("could not auto-detect mountpoint for %s (pass --mountpoint): %w", o.Device, err)
	}
	mp := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if mp == "" {
		return "", fmt.Errorf("no mountpoint found for %s", o.Device)
	}
	return mp, nil
}

func prepareFsfreeze(o Options) (string, func(), error) {
	mp, err := mountpointFor(o)
	if err != nil {
		return "", func() {}, err
	}
	if err := runShell(o.PreHook); err != nil {
		return "", func() {}, err
	}
	log.Printf("snapshot: freezing %s (writes blocked until read completes)", mp)
	if out, err := run("fsfreeze", "-f", mp); err != nil {
		return "", func() {}, fmt.Errorf("fsfreeze freeze: %w (%s)", err, out)
	}
	// Resume the app immediately; the freeze itself guarantees consistency.
	if err := runShell(o.PostHook); err != nil {
		_, _ = run("fsfreeze", "-u", mp) // best-effort thaw before bailing
		return "", func() {}, err
	}
	cleanup := func() {
		log.Printf("snapshot: thawing %s", mp)
		if out, err := run("fsfreeze", "-u", mp); err != nil {
			log.Printf("snapshot: WARNING thaw failed for %s: %v (%s)", mp, err, out)
		}
	}
	return o.Device, cleanup, nil
}

func prepareLVM(o Options) (string, func(), error) {
	if o.LVMSnapSize == "" {
		o.LVMSnapSize = "5G"
	}
	name := o.SnapName
	if name == "" {
		name = fmt.Sprintf("vmrepl-snap-%d", time.Now().Unix())
	}

	// Freeze just long enough to take the CoW snapshot, then thaw.
	mp, mpErr := mountpointFor(o)
	if err := runShell(o.PreHook); err != nil {
		return "", func() {}, err
	}
	thaw := func() {}
	if mpErr == nil {
		if out, err := run("fsfreeze", "-f", mp); err != nil {
			log.Printf("snapshot: WARNING could not freeze %s before snapshot: %v (%s)", mp, err, out)
		} else {
			thaw = func() { _, _ = run("fsfreeze", "-u", mp) }
		}
	}

	out, err := run("lvcreate", "--size", o.LVMSnapSize, "--snapshot", "--name", name, o.Device)
	thaw()                   // unfreeze immediately after the instant snapshot
	_ = runShell(o.PostHook) // resume the app
	if err != nil {
		return "", func() {}, fmt.Errorf("lvcreate snapshot: %w (%s)", err, out)
	}

	// The snapshot device path: /dev/<vg>/<name>. Derive vg from the origin.
	snapPath, derr := lvSnapshotPath(o.Device, name)
	if derr != nil {
		_, _ = run("lvremove", "-f", "/dev/"+name)
		return "", func() {}, derr
	}
	log.Printf("snapshot: created LVM snapshot %s of %s (size %s)", snapPath, o.Device, o.LVMSnapSize)

	cleanup := func() {
		log.Printf("snapshot: removing LVM snapshot %s", snapPath)
		if out, err := run("lvremove", "-f", snapPath); err != nil {
			log.Printf("snapshot: WARNING lvremove failed for %s: %v (%s)", snapPath, err, out)
		}
	}
	return snapPath, cleanup, nil
}

// lvSnapshotPath turns an origin LV path (/dev/vg/lv or /dev/mapper/vg-lv) plus
// a snapshot name into the snapshot's device path under the same volume group.
func lvSnapshotPath(origin, snapName string) (string, error) {
	out, err := run("lvs", "--noheadings", "-o", "vg_name", origin)
	if err != nil {
		return "", fmt.Errorf("resolve volume group for %s: %w", origin, err)
	}
	vg := strings.TrimSpace(out)
	if vg == "" {
		return "", fmt.Errorf("empty volume group for %s", origin)
	}
	return fmt.Sprintf("/dev/%s/%s", vg, snapName), nil
}
