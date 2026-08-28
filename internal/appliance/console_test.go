package appliance

import (
	"strings"
	"testing"

	"github.com/tiny125/vm-replication/internal/api"
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

// The guided cutover must take a consistent final pass by default
// (skip_snapshot=false → the appliance's existing remount-ro/LVM quiesce runs),
// with an opt-out checkbox for an already-powered-off source. This is the fix
// for the "converted disk has no root filesystem" fsck failure caused by
// cloning a live, inconsistent block image.
func TestConsoleBlockCutoverQuiesces(t *testing.T) {
	js := extractJSFunc(t, "async function startMig(")
	if !strings.Contains(js, "checkboxes:[{id:'skip_snap'") {
		t.Error("cutover must offer a skip-the-snapshot opt-out checkbox")
	}
	if !strings.Contains(js, "skipSnap=!!r.skip_snap") {
		t.Error("skip_snapshot must come from the checkbox value")
	}
	if !strings.Contains(js, "skip_snapshot:skipSnap") {
		t.Error("cutover must post the computed skip_snapshot value")
	}
	// The dialog must mention the read-only quiesce so the flow is honest.
	if !strings.Contains(js, "read-only") {
		t.Error("cutover dialog should describe the read-only quiesce")
	}
	// A busy root must not dead-end the cutover: the dialog promises the
	// automatic fallback to the crash-consistent data (fsck-repaired, validated
	// before power-off) instead of the old fail-fast-and-retry loop.
	if !strings.Contains(js, "automatically falls back") {
		t.Error("cutover dialog must say a busy root automatically falls back to the crash-consistent data")
	}
	if strings.Contains(js, "the cutover fails fast and asks you to stop them") {
		t.Error("the old fail-fast wording must be gone — the cutover no longer aborts on a busy root")
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

// Product decision: Linode's own Backups service, Images, and cross-DC
// migration don't work with Block Storage volumes, so a volume-boot machine
// sits outside Linode's lifecycle tooling for its whole life. The console
// therefore now offers ONLY local-disk boot from the create form — but the
// underlying code path (api.BootTargetVolume, the HTTP API, and rendering of
// pre-existing volume-boot migrations) stays fully intact so the method can be
// re-enabled later purely by editing code. This test asserts the create-form
// side of that: exactly one method, and it posts boot_target:'disk'.
func TestConsoleOffersOnlyDiskBootMethod(t *testing.T) {
	// One method, so the form states it rather than offering a picker. What
	// matters is that no OTHER method is presented as selectable, and that the
	// operator is told what this one does.
	if strings.Contains(consoleHTML, `id="m_method"`) {
		t.Error("the method picker should be gone — with a single method the form should state it, not offer a control")
	}
	if !strings.Contains(consoleHTML, "Local disk (NVMe boot)") {
		t.Error("the create form must name the migration method it will use")
	}
	// No other boot target may be offered as a choice anywhere in the form.
	if strings.Contains(consoleHTML, `<option value="volume"`) {
		t.Error("volume boot must not appear as a selectable option")
	}
	if strings.Contains(consoleHTML, `<option value="file"`) {
		t.Error("file transfer must not appear as a selectable option")
	}
}

func TestConsoleNoFileTransferTraces(t *testing.T) {
	for _, gone := range []string{
		`value="file"`, "m_osimage", "m_used", "os_image:",
		"/api/v1/linode/images", "loadImages", "fileFields",
		"function destPanel(", "async function createDest(", "/destination",
	} {
		if strings.Contains(consoleHTML, gone) {
			t.Errorf("console still references removed file-transfer piece %q", gone)
		}
	}
}

// Creating a migration from the console must always send boot_target:'disk'.
// With one method there is no element to read, so method() returns it directly
// — this checks the plumbing stays wired from method() through to the request.
func TestConsoleCreateSendsDiskBootTarget(t *testing.T) {
	methodFn := extractJSFunc(t, "function method(){")
	if !strings.Contains(methodFn, "'disk'") {
		t.Error("method() must resolve to 'disk' so createMig posts the right boot target")
	}
	createFn := extractJSFunc(t, "async function createMig(")
	if !strings.Contains(createFn, "boot_target:mth") {
		t.Error("createMig must send boot_target computed from method()")
	}
	if !strings.Contains(createFn, "mth=method()") && !strings.Contains(createFn, "mth = method()") {
		t.Error("createMig must derive mth from method()")
	}
}

// A migration created (e.g. in earlier testing) before the console stopped
// OFFERING volume boot can still exist in the database with
// boot_target:"volume". The detail view must keep recognizing it as a
// supported method — with its own banner — not fall into the "unsupported
// migration method" branch reserved for genuinely removed methods (like the
// old file-transfer method), and the cutover dialog's volume-naming field
// must still work for it.
func TestConsoleRendersExistingVolumeBootMigration(t *testing.T) {
	js := extractJSFunc(t, "function migCard(")
	if !strings.Contains(js, `m.boot_target===''||m.boot_target==='volume'`) {
		t.Error("migCard must still explicitly recognize boot_target 'volume' as a known, supported method")
	}
	if !strings.Contains(js, "Boot: separate Block Storage volume") {
		t.Error("migCard must still render the volume-boot banner for existing volume-boot migrations")
	}
	// vol_name is used by BOTH volume boot (naming the boot volume) and
	// multi-disk local-disk boot (naming the data volumes) — it must not have
	// been removed while stripping volume-boot-only controls.
	startJS := extractJSFunc(t, "async function startMig(")
	if !strings.Contains(startJS, "id:'vol_name'") {
		t.Error("cutover dialog must still offer vol_name — used by volume boot AND multi-disk local-disk boot")
	}
	if !strings.Contains(startJS, "!disk||nDisks>1") {
		t.Error("vol_name must still be shown for volume boot (!disk) or multi-disk local-disk boot (nDisks>1)")
	}
}

// A migration row can still carry a boot_target the console no longer
// supports (e.g. 'file', from before the file-transfer method was removed —
// the column stays on old databases). The card must show that raw value
// plainly in the method banner instead of silently mislabeling it as one of
// the current methods, and it must not crash rendering the rest of the card.
func TestConsoleUnknownBootTargetDegradesGracefully(t *testing.T) {
	js := extractJSFunc(t, "function migCard(")
	if !strings.Contains(js, "m.boot_target===''||m.boot_target==='volume'") {
		t.Error("migCard must recognize the known boot targets ('' and 'volume') explicitly, not just fall through")
	}
	if !strings.Contains(js, "Unsupported migration method") {
		t.Error("migCard must show a distinct banner for an unrecognized/legacy boot_target, naming it")
	}
	if !strings.Contains(js, "Unsupported migration method: '+esc(m.boot_target)") {
		t.Error("the unsupported-method banner must render the RAW boot_target value (escaped), not hide it")
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
	// The Refresh button re-checks the audit bucket's real existence — the fix
	// for a console that falsely shows the bucket gone. It spins and toasts like
	// the others, and awaits the settings reload.
	refresh := extractJSFunc(t, "async function refreshAuditBucket(")
	for needle, why := range map[string]string{
		"busy(btn,true)":                        "refresh must spin its button",
		"/api/v1/settings/audit-bucket/refresh": "refresh must call the re-check endpoint",
		"await loadSettings(":                   "refresh must await the settings reload",
		"busy(btn,false)":                       "refresh must clear the spinner",
	} {
		if !strings.Contains(refresh, needle) {
			t.Errorf("refreshAuditBucket %s (missing %q)", why, needle)
		}
	}
	if !strings.Contains(consoleHTML, `onclick="refreshAuditBucket(this)"`) {
		t.Error("the Linode automation card must render a Refresh button")
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

// The console offers local-disk boot only, so the Source check tab must not
// hand the operator a verdict for a method the create form does not offer.
// "Volume boot: Supported" beside a form with no volume option is a dead end —
// it reads as a recommendation the product then refuses to honour.
//
// The API deliberately still RETURNS every verdict it can compute (that is what
// keeps the volume-boot assessment logic under test); the filtering is the
// console's job, so re-enabling a method is a one-line change to OFFERED.
func TestConsoleSourceCheckShowsOnlyOfferedMethods(t *testing.T) {
	js := extractJSFunc(t, "function renderSourceCheck(")
	if !strings.Contains(js, "const OFFERED=['disk']") {
		t.Error("the source-check table must filter verdicts to the methods the console can create")
	}
	if !strings.Contains(js, "const shown=(a.methods||[]).filter(") {
		t.Error("the rendered rows must come from the filtered set, not the raw API list")
	}
	// The bottom-line verdict must agree with the filtered table: if it still
	// consulted every method, a source that only volume boot could handle would
	// say "can migrate" above a table showing no usable method.
	if !strings.Contains(js, "const okAny=shown.some(") {
		t.Error("the overall verdict must be computed from the shown methods, not all of them")
	}
}

// There is one migration method, so the create form should state it, not offer
// a choice. A disabled <select> still reads as a control the operator might be
// able to change — it invites a click, and a dropdown with a single greyed-out
// entry looks like something is broken or unavailable to them.
func TestConsoleShowsMethodAsTextNotADropdown(t *testing.T) {
	if strings.Contains(consoleHTML, `<select id="m_method"`) {
		t.Error("the migration method must be presented as text, not a <select> — there is only one method to offer")
	}
	// The value still has to reach createMig, so method() must keep working
	// without the element.
	js := extractJSFunc(t, "function method(")
	if !strings.Contains(js, "'disk'") {
		t.Errorf("method() must still resolve to 'disk'; got: %s", js)
	}
	// And the operator should be told what the method actually does.
	for _, want := range []string{"local", "NVMe"} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("the method description should mention %q so the operator knows what they get", want)
		}
	}
}

// Nothing in the create form may reset or read a method element that no longer
// exists — a stale $('m_method').value would throw and break the form reset.
func TestConsoleNoDanglingMethodElementReferences(t *testing.T) {
	if strings.Contains(consoleHTML, "$('m_method')") {
		t.Error("console still references the removed #m_method element; the create-form reset would throw")
	}
}

// Every command the console hands an operator embeds the console's own URL.
// Once the default port became 443 those read
// "https://198.51.100.7:443/check/source.sh?token=…" — the port is redundant
// for the scheme and is pure noise in a line someone has to read, trust and
// paste into a production server.
func TestConsoleBaseOmitsTheDefaultPort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scheme string
		host   string
		port   int
		want   string
	}{
		{"https on 443 drops the port", "https", "198.51.100.7", 443, "https://198.51.100.7"},
		{"http on 80 drops the port", "http", "198.51.100.7", 80, "http://198.51.100.7"},
		{"a non-default port is kept", "https", "198.51.100.7", 8080, "https://198.51.100.7:8080"},
		{"80 under https is NOT default and is kept", "https", "198.51.100.7", 80, "https://198.51.100.7:80"},
		{"443 under http is NOT default and is kept", "http", "198.51.100.7", 443, "http://198.51.100.7:443"},
		{"hostnames work the same", "https", "appliance.internal", 443, "https://appliance.internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := consoleBaseURL(tc.scheme, tc.host, tc.port); got != tc.want {
				t.Errorf("consoleBaseURL(%q,%q,%d) = %q, want %q", tc.scheme, tc.host, tc.port, got, tc.want)
			}
		})
	}
}

// The generated commands must actually use it, or the helper is decoration.
func TestGeneratedCommandsUseTheSharedBase(t *testing.T) {
	s := &Server{cfg: Config{PublicHost: "198.51.100.7", ConsolePort: 443}}
	for name, got := range map[string]string{
		"enroll":    s.enrollCmd("tok", api.Migration{}),
		"uninstall": s.uninstallCmd(),
	} {
		if strings.Contains(got, ":443/") {
			t.Errorf("%s command still carries a redundant :443 — %s", name, got)
		}
		if !strings.Contains(got, "https://198.51.100.7/") {
			t.Errorf("%s command should address the console without a port — %s", name, got)
		}
	}
}
