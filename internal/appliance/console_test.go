package appliance

import (
	"strings"
	"testing"
)

// The console's auto-refresh timers (5s full refresh + 1s live progress/status
// poll + 1s elapsed ticker) must be started by start(), which BOTH entry paths
// run: a page load with an existing session (init) and an explicit sign-in
// through the login form. They used to be registered only on the init path, so
// after an appliance restart (sessions wiped → login form) the console had NO
// auto-refresh until a manual page reload — progress only moved when the
// operator clicked Refresh.
func TestConsoleTimersStartOnBothLoginPaths(t *testing.T) {
	if !strings.Contains(consoleHTML, "function startTimers()") {
		t.Fatal("console must define startTimers() so timer registration is shared by both login paths")
	}
	// start() is the common path (init success AND login form) — it must start
	// the timers.
	startFn := extractJSFunc(t, "async function start()")
	if !strings.Contains(startFn, "startTimers()") {
		t.Error("start() must call startTimers() — otherwise signing in through the login form leaves the console without auto-refresh")
	}
	// startTimers must be idempotent (guarded), since start() runs on every
	// sign-in and timers must not stack.
	timersFn := extractJSFunc(t, "function startTimers()")
	if !strings.Contains(timersFn, "timersOn") {
		t.Error("startTimers() must guard against double registration")
	}
	for _, want := range []string{"refresh(false)},5000", "},1000"} {
		if !strings.Contains(timersFn, want) {
			t.Errorf("startTimers() should register the interval %q", want)
		}
	}
}

// The disk-boot cutover needs the operator to paste ONE command into the
// rescue'd instance's Lish console. While that copy is pending the backend sets
// cutover_copy_cmd on the migration view, and the card must render it as an
// "action needed" banner with the command and a Copy button.
func TestConsoleRendersCutoverCopyCommand(t *testing.T) {
	for _, want := range []string{
		"cutover_copy_cmd", // the view field is consumed
		"Action needed",    // the banner headline
		"cutcmd",           // the <pre> id the Copy button reads
		"/lish/weblish",    // direct link to the instance's web Lish console
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console should render the cutover copy step (missing %q)", want)
		}
	}
}

// The Start-replication confirmation must be method-aware. File transfer copies
// FILES onto a launched destination — it must NOT tell the operator "the agent
// streams every block" (that's the block-method wording and confused users into
// thinking no destination existed). The file variant must mention launching the
// destination and copying files.
func TestConsoleStartDialogIsMethodAware(t *testing.T) {
	js := extractJSFunc(t, "async function startReplication(")
	// It must branch on the migration's method.
	if !strings.Contains(js, "boot_target") {
		t.Error("startReplication must pick its wording from the migration's boot_target")
	}
	// File-transfer wording present…
	for _, want := range []string{"launches the destination", "used files"} {
		if !strings.Contains(js, want) {
			t.Errorf("file-transfer Start dialog missing %q", want)
		}
	}
	// …and the block wording still present for block methods.
	if !strings.Contains(js, "streams every block") {
		t.Error("block-method Start dialog should keep its 'streams every block' wording")
	}
}

// The file-transfer flow must render file-appropriate wording (not block-method
// vocabulary) across the card: the completion banner, the guided-cutover freeze
// and awaiting-cutover banners, and the action-button tooltips all carry a
// file-specific branch. This guards the message sweep so block wording can't
// silently creep back into the file path.
func TestConsoleFileFlowMessagesAreMethodAware(t *testing.T) {
	for _, want := range []string{
		"Your files were copied onto Linode",       // file completion banner branch
		"Finishing the last file-copy pass",        // file freeze banner
		"the copied files are held for launch",     // file awaiting-cutover banner
		"reboots the already-launched destination", // file cutover button tooltip
		"initial file copy complete",               // file validation-check explainer
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("file-transfer flow missing method-aware wording %q", want)
		}
	}
}

// The file-transfer flow must render the explicit "Create destination instance"
// step: a destPanel that keys off dest_state, a createDest() that collects a name
// + root password and POSTs to /destination, and a Start button whose disabled
// title points the operator at the destination step.
func TestConsoleDestinationStep(t *testing.T) {
	for _, want := range []string{
		"function destPanel(",         // the destination status panel
		"v.dest_state",                // it keys off the destination state
		"Create destination instance", // the button label
		"async function createDest(",  // the create action
		"/destination",                // POSTs to the new endpoint
		"root_password:r.d_pw",        // sends the operator's root password
		"dest_manual_cmd",             // renders the manual-install fallback
		"the destination is ready",    // Start tooltip mentions destination readiness
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("destination step missing %q", want)
		}
	}
	// The card poller must rebuild when the destination status changes.
	if !strings.Contains(consoleHTML, "card.dataset.dest") {
		t.Error("card must track dest_state so the panel + Start gate update live")
	}
}

// The guided cutover must tell the operator, ON THE CARD, when it is safe to
// power off the source: a "keep the source running" banner while step 1's
// freeze/convert/validate runs (cutover_freezing), then a "safe to power off the
// source server" action banner once the image is validated (awaiting_cutover).
// Same visual style as the disk-copy action box, and no emoji icons.
func TestConsoleCutoverGuidance(t *testing.T) {
	for _, want := range []string{
		"cutover_freezing",                    // freeze-in-progress view flag is consumed
		"keep the source server running",      // the wait guidance
		"safe to power off the source server", // the go-ahead action banner
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console should render the cutover guidance (missing %q)", want)
		}
	}
	if strings.Contains(consoleHTML, "⚡") {
		t.Error("action banners must not carry the lightning icon")
	}
}

// The guided cutover of a BLOCK migration must take a consistent final pass by
// default (skip_snapshot=false → the appliance's existing remount-ro/LVM quiesce
// runs), with an opt-out checkbox for an already-powered-off source. File
// transfer must be untouched: it keeps skip_snapshot=true and has no checkbox.
// This is the fix for the "converted disk has no root filesystem" fsck failure
// caused by cloning a live, inconsistent block image.
func TestConsoleBlockCutoverQuiesces(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	// Block methods get the opt-out checkbox (guarded by !file) …
	if !strings.Contains(js, "if(!file)opts.checkboxes") || !strings.Contains(js, "skip_snap") {
		t.Error("block cutover must offer a skip-the-snapshot opt-out checkbox")
	}
	// … and send skip_snapshot method-aware: true for file, the checkbox for block.
	if !strings.Contains(js, "file?true:!!r.skip_snap") {
		t.Error("skip_snapshot must be true for file and the checkbox value for block methods")
	}
	if !strings.Contains(js, "skip_snapshot:skipSnap") {
		t.Error("cutover must post the computed skip_snapshot value")
	}
	// The dialog must mention the read-only quiesce so the block flow is honest.
	if !strings.Contains(js, "read-only") {
		t.Error("cutover dialog should describe the read-only quiesce for block methods")
	}
}

// File-transfer cutover only reboots the destination that was already created
// (and named/credentialed) at "Create destination instance" — so its cutover
// dialog must show NONE of the instance-name / volume-name / root-password /
// SSH-key fields (those only apply to the block methods, which create the
// instance at cutover). Guards a stress-test regression where file showed a
// "New volume name" field for a method that has no volume.
func TestConsoleFileCutoverOmitsBlockFields(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	if !strings.Contains(js, "if(!file){") {
		t.Error("cutover dialog must gate its optional fields behind !file")
	}
	// The vol_name field must be doubly gated (volume-boot only).
	if !strings.Contains(js, "if(!disk)fields.push({id:'vol_name'") {
		t.Error("vol_name field must be present for volume boot")
	}
	// The credential note (access) must be suppressed for file.
	if !strings.Contains(js, "html:how+(file?'':access)+prep") {
		t.Error("the root-password note must be suppressed for the file cutover dialog")
	}
}

// The cutover dialog lets the operator NAME the launched instance (both boot
// methods) and the cutover volume (volume-boot only), sent as label /
// volume_label on the /start request; blank falls back to <name>-cutover.
func TestConsoleCutoverNamingFields(t *testing.T) {
	for _, want := range []string{
		"inst_name",         // instance-name dialog field
		"vol_name",          // volume-name dialog field (volume-boot)
		"volume_label:",     // sent to the API
		"label:r.inst_name", // instance name sent to the API
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console should offer cutover naming (missing %q)", want)
		}
	}
}

// The create card must offer all three methods from ONE selector, defaulting to
// file transfer, and file mode must expose the OS-image dropdown + used-storage
// field and post boot_target:'file' with os_image.
func TestConsoleMigrationMethodSelector(t *testing.T) {
	for _, want := range []string{
		`id="m_method"`,         // single method selector
		`value="file"`,          // file option
		`value="volume"`,        // block volume option
		`value="disk"`,          // block disk option
		"m_osimage",             // destination OS image dropdown
		"m_used",                // used-storage input (file mode)
		"boot_target:mth",       // create posts the chosen method
		"os_image:",             // create posts the OS image
		"/api/v1/linode/images", // images are loaded for the dropdown
		"loadImages",            // image loader
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("create card should support the method selector (missing %q)", want)
		}
	}
	// File must be the default selected option (its <option> carries selected).
	if !strings.Contains(consoleHTML, `value="file" selected`) {
		t.Error("file transfer must be the default selected method")
	}
}

// The source-details helper command must also report the OS and used storage so
// the operator can size the plan by used data and match the destination OS.
func TestConsoleSourceHelperReportsOSAndUsed(t *testing.T) {
	for _, want := range []string{"os-release", "Used"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("source-details helper should report %q", want)
		}
	}
}

// The source-details helper lists whole disks via lsblk. It must skip pseudo
// block devices (nbd/loop/ram/zram/sr/fd) and zero-size nodes so a destination
// (or any host) with the nbd kernel module loaded doesn't print 16 empty
// "/dev/nbdN — Size 0" lines that look like extra disks.
func TestConsoleSourceHelperSkipsPseudoDisks(t *testing.T) {
	i := strings.Index(consoleHTML, `id="srcCmd"`)
	if i < 0 {
		t.Fatal("source-details helper (srcCmd) not found")
	}
	cmd := consoleHTML[i:]
	if end := strings.Index(cmd, "</pre>"); end >= 0 {
		cmd = cmd[:end]
	}
	// The lsblk|awk pipeline must filter by size and by device-name prefix.
	if !strings.Contains(cmd, "$2>0") {
		t.Error("helper must skip zero-size block devices ($2>0)")
	}
	for _, pseudo := range []string{"nbd", "loop", "ram", "zram", "sr", "fd"} {
		if !strings.Contains(cmd, pseudo) {
			t.Errorf("helper must exclude pseudo device %q from the disk list", pseudo)
		}
	}
}

// The destructive Settings buttons — "Delete audit bucket" and "Remove token"
// — must give clear live feedback: a spinner on the button for the WHOLE
// operation (the delete request AND the settings reload that follows), then a
// top-right toast confirming the removal. The spinner must not drop before the
// card re-renders, so each handler awaits loadSettings() inside the try before
// the finally clears busy(). The supporting CSS/DOM (the .busy spinner and the
// #toasts top-right container) must be present for that feedback to show.
func TestConsoleDeleteButtonsSpinAndToast(t *testing.T) {
	// Shared infrastructure: the spinner animation and the top-right toast host.
	for _, want := range []string{
		"button.busy::after", // the spinning-circle pseudo-element
		"@keyframes spin",    // its rotation
		`id="toasts"`,        // the top-right toast container...
		".toast-wrap{position:fixed;top:18px;right:18px", // ...pinned top-right
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console is missing spinner/toast infrastructure %q", want)
		}
	}
	for _, fn := range []string{"async function deleteAuditBucket(", "async function removeToken("} {
		body := extractJSFunc(t, fn)
		checks := map[string]string{
			"busy(btn,true)":      "must start the button spinner before the delete request",
			"await api('DELETE'":  "must await the DELETE call (spinner shows for the whole delete period)",
			"await loadSettings(": "must await the settings reload so the spinner persists until the card re-renders",
			",'ok')":              "must raise a success toast (top-right notification) once removed",
			"busy(btn,false)":     "must clear the spinner in finally",
		}
		for needle, why := range checks {
			if !strings.Contains(body, needle) {
				t.Errorf("%s %s (missing %q)", fn, why, needle)
			}
		}
		// The toast must fire before the reload so the notification appears the
		// instant the delete succeeds, not after the extra GET round-trip.
		if ti, li := strings.Index(body, "toast("), strings.Index(body, "loadSettings("); ti < 0 || li < 0 || ti > li {
			t.Errorf("%s must toast() before loadSettings()", fn)
		}
	}
}

// extractJSFunc returns the source of the embedded-JS function that begins with
// header, up to the next top-level (column-0) "function"/"async function"
// declaration — enough to assert what a given function contains.
func extractJSFunc(t *testing.T, header string) string {
	t.Helper()
	i := strings.Index(consoleHTML, header)
	if i < 0 {
		t.Fatalf("console JS does not define %q", header)
	}
	rest := consoleHTML[i+len(header):]
	end := len(rest)
	for _, next := range []string{"\nfunction ", "\nasync function ", "\ninit();"} {
		if j := strings.Index(rest, next); j >= 0 && j < end {
			end = j
		}
	}
	return rest[:end]
}
