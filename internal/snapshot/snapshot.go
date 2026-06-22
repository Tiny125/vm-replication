// Package snapshot establishes a consistent point-in-time view of a source
// device before the agent reads it, so the replicated image is consistent
// rather than merely crash-consistent.
//
// Modes:
//
//   - none:     read the live device directly (crash-consistent).
//   - fsfreeze: flush and *briefly* FIFREEZE the filesystem, then thaw
//     immediately and read live. fsfreeze can only make an instantaneous
//     snapshot consistent; holding the freeze across a whole-device read blocks
//     every write on the filesystem (root included) and deadlocks the agent's
//     own manifest write — so we never hold it. This mode is therefore a
//     best-effort flush, not a true point-in-time image.
//   - lvm:      take an LVM copy-on-write snapshot of the origin LV and read
//     from that. The freeze is held only for the instant the snapshot is
//     created; the source keeps running while we replicate from the stable
//     snapshot. This is the only true point-in-time (crash-consistent) mode.
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
	"sync"
	"time"
)

// Mode selects the consistency strategy.
type Mode string

const (
	ModeNone     Mode = "none"
	ModeFsfreeze Mode = "fsfreeze"
	ModeLVM      Mode = "lvm"
	// ModeRemountRO remounts the source filesystem read-only for the read, giving a
	// genuinely consistent (clean, not just crash-consistent) image without LVM.
	// Used at cutover for non-LVM sources that are being decommissioned: writes are
	// blocked for the duration, and cleanup remounts read-write again so an aborted
	// cutover doesn't leave the source crippled.
	ModeRemountRO Mode = "remountro"
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
// copy-on-write snapshot when device is an LVM logical volume (a true
// point-in-time image with no source downtime), otherwise ModeNone. It
// deliberately does NOT fall back to fsfreeze: fsfreeze cannot snapshot a
// whole-device read, and holding a freeze for the duration would block all
// writes and wedge the source (root filesystems included). Used at cutover when
// the operator hasn't pinned a -snapshot mode; on ModeNone the caller reads live
// and the appliance proceeds on the current data (with a warning).
func DetectMode(device string) Mode {
	if isLVM(device) {
		return ModeLVM
	}
	return ModeNone
}

// isLVM reports whether device is an LVM logical volume (so an LVM snapshot is
// possible). It is best-effort: if lvs is absent or errors, we treat the device
// as non-LVM (no point-in-time snapshot available).
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
	case ModeRemountRO:
		return prepareRemountRO(o)
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

// maxFreezeHold bounds how long a filesystem may stay frozen. A watchdog
// force-thaws after this so that a hang between freeze and the intended thaw
// (e.g. lvcreate stalls, or the process is killed) can never leave the source
// filesystem — often root — frozen and wedge the entire box.
const maxFreezeHold = 60 * time.Second

// freeze FIFREEZEs mp and returns an idempotent thaw func. A watchdog
// force-thaws after maxFreezeHold regardless, as a safety net against a hang
// leaving the filesystem frozen. Callers must still call thaw() as soon as the
// instantaneous work (e.g. snapshot creation) is done — the freeze must never be
// held across a long device read.
func freeze(mp string) (thaw func(), err error) {
	if out, ferr := run("fsfreeze", "-f", mp); ferr != nil {
		return func() {}, fmt.Errorf("fsfreeze freeze %s: %w (%s)", mp, ferr, out)
	}
	var once sync.Once
	done := make(chan struct{})
	thaw = func() {
		once.Do(func() {
			close(done)
			if out, uerr := run("fsfreeze", "-u", mp); uerr != nil {
				log.Printf("snapshot: WARNING thaw failed for %s: %v (%s)", mp, uerr, out)
			}
		})
	}
	go func() {
		select {
		case <-time.After(maxFreezeHold):
			log.Printf("snapshot: WARNING freeze watchdog fired after %s; force-thawing %s to avoid wedging the source", maxFreezeHold, mp)
			thaw()
		case <-done:
		}
	}()
	return thaw, nil
}

// prepareFsfreeze flushes the filesystem with a brief freeze, then thaws
// immediately and reads the device live. It never holds the freeze across the
// read (that would block all writes and self-deadlock on the manifest write), so
// it is a best-effort flush rather than a true point-in-time snapshot — use
// ModeLVM for crash-consistency.
func prepareFsfreeze(o Options) (string, func(), error) {
	mp, err := mountpointFor(o)
	if err != nil {
		return "", func() {}, err
	}
	if err := runShell(o.PreHook); err != nil {
		return "", func() {}, err
	}
	log.Printf("snapshot: flushing %s with a brief freeze, then reading live (fsfreeze is not a point-in-time snapshot; use LVM for crash-consistency)", mp)
	thaw, err := freeze(mp)
	if err != nil {
		return "", func() {}, err
	}
	thaw() // immediately — never hold the freeze across the read
	_ = runShell(o.PostHook)
	return o.Device, func() {}, nil
}

// prepareRemountRO flushes and remounts the filesystem read-only so a whole-device
// read is consistent (no in-flight writes), with no LVM required. cleanup remounts
// read-write and runs the post-hook, so an aborted cutover restores the source.
// Intended for cutover on a source that will be powered off afterwards.
func prepareRemountRO(o Options) (string, func(), error) {
	mp, err := mountpointFor(o)
	if err != nil {
		return "", func() {}, err
	}
	if err := runShell(o.PreHook); err != nil {
		return "", func() {}, err
	}
	_, _ = run("sync")
	log.Printf("snapshot: remounting %s read-only for a consistent cutover read", mp)
	if out, rerr := run("mount", "-o", "remount,ro", mp); rerr != nil {
		_ = runShell(o.PostHook)
		return "", func() {}, fmt.Errorf("remount %s read-only failed (a process is still writing to it — stop your apps/services first): %w (%s)", mp, rerr, out)
	}
	cleanup := func() {
		if out, uerr := run("mount", "-o", "remount,rw", mp); uerr != nil {
			log.Printf("snapshot: WARNING could not remount %s read-write again: %v (%s)", mp, uerr, out)
		}
		_ = runShell(o.PostHook)
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

	// Freeze just long enough to take the CoW snapshot, then thaw. The watchdog
	// in freeze() force-thaws if lvcreate hangs, so the source can never stay
	// frozen.
	mp, mpErr := mountpointFor(o)
	if err := runShell(o.PreHook); err != nil {
		return "", func() {}, err
	}
	thaw := func() {}
	if mpErr == nil {
		if t, err := freeze(mp); err != nil {
			log.Printf("snapshot: WARNING could not freeze %s before snapshot: %v", mp, err)
		} else {
			thaw = t
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
