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

// The guided cutover must tell the operator, ON THE CARD, when it is safe to
// power off the source: a "keep the source running" banner while step 1's
// freeze/drain runs (cutover_freezing), then a "power off the source server
// now" action banner once frozen (awaiting_cutover). Same visual style as the
// disk-copy action box, and no emoji icons.
func TestConsoleCutoverGuidance(t *testing.T) {
	for _, want := range []string{
		"cutover_freezing",                // freeze-in-progress view flag is consumed
		"keep the source server running",  // the wait guidance
		"power off the source server now", // the go-ahead action banner
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console should render the cutover guidance (missing %q)", want)
		}
	}
	if strings.Contains(consoleHTML, "⚡") {
		t.Error("action banners must not carry the lightning icon")
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
