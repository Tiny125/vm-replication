package appliance

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The documentation site must be served at /documentation WITHOUT a console
// session (it's the public how-to guide reached at https://<ip>:<port>/documentation),
// and must cover the full console journey: install, sign-in, the Linode API
// token, and creating a migration. The guide now documents ONLY local-disk
// boot (product decision: volume boot no longer offered in the console — see
// TestDocsGuideIsSingleMethod below), even though the underlying code and API
// still support it.
func TestDocsSiteServed(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDocs(rr, httptest.NewRequest("GET", "/documentation", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /documentation: %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type %q, want text/html", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		// Core journey sections.
		"Install the replication server", "Sign in", "Linode API token",
		"Create a migration", "Disk boot",
		"Enroll the source server", "Start replication", "Cutover",
		"Troubleshooting",
		// Screenshots are embedded and referenced.
		"/documentation/img/console-overview.png",
		"/documentation/img/new-migration.png",
		"/documentation/img/settings-token.png",
		"/documentation/img/migration-card.png",
		// Styled button references (the reader sees the button as it looks).
		"btn-demo",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("documentation missing %q", want)
		}
	}
	// The removed file-transfer method must leave no trace in the guide.
	for _, gone := range []string{"File transfer", "file-transfer", "file receiver", "OS image"} {
		if strings.Contains(body, gone) {
			t.Errorf("documentation still references the removed file-transfer method (%q)", gone)
		}
	}
	// No icons: the guide must not use emoji-style icons.
	for _, banned := range []string{"⚡", "🚀", "📘", "💡"} {
		if strings.Contains(body, banned) {
			t.Errorf("documentation must not use icons/emoji (found %q)", banned)
		}
	}
}

// The in-app guide must read as SINGLE-METHOD documentation: no "Volume
// boot" section/heading, no two-method comparison table, no "choose a
// migration method" section, and no leftover "both methods"/"either
// method"/"the two methods" phrasing or dangling #choose-method /
// #volume-boot cross-references. This is a console/documentation change, not
// a code-removal — the volume-boot API and code path are untouched and
// covered separately (see TestCreateMigrationAcceptsVolumeBootTarget in
// boot_target_test.go).
func TestDocsGuideIsSingleMethod(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDocs(rr, httptest.NewRequest("GET", "/documentation", nil))
	body := rr.Body.String()
	for _, gone := range []string{
		"Volume boot", "volume-boot", "Choose a migration method",
		"choose-method", "both methods", "either method", "the two methods",
		"two block-for-block migration methods", "two remaining",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("documentation should read as single-method (disk boot only) — still contains %q", gone)
		}
	}
}

// Screenshots are embedded in the binary and served under /documentation/img/.
func TestDocsImagesServed(t *testing.T) {
	s := &Server{}
	for _, name := range []string{
		// method-selector.png and source-check.png were dropped: both showed the
		// old three-method picker (File transfer / Volume boot / Disk boot),
		// and the console now offers local-disk boot only. Shipping a stale
		// screenshot of a control that no longer exists is worse than none.
		"console-overview.png", "login.png", "new-migration.png",
		"settings-token.png", "migration-card.png",
		"source-helper.png",
	} {
		rr := httptest.NewRecorder()
		s.handleDocsImage(rr, httptest.NewRequest("GET", "/documentation/img/"+name, nil))
		if rr.Code != 200 {
			t.Errorf("GET img %s: %d, want 200", name, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
			t.Errorf("img %s content type %q, want image/png", name, ct)
		}
		if rr.Body.Len() < 1000 {
			t.Errorf("img %s suspiciously small (%d bytes)", name, rr.Body.Len())
		}
	}
	// Unknown / traversal paths are rejected.
	for _, bad := range []string{"nope.png", "../console.go", "a/b.png", "method-selector.png", "source-check.png"} {
		rr := httptest.NewRecorder()
		s.handleDocsImage(rr, httptest.NewRequest("GET", "/documentation/img/"+bad, nil))
		if rr.Code == 200 {
			t.Errorf("img %q must not be served", bad)
		}
	}
}

// The console links to the documentation so operators can find the guide.
func TestConsoleLinksDocumentation(t *testing.T) {
	if !strings.Contains(consoleHTML, `href="/documentation"`) {
		t.Error("console should link to /documentation")
	}
}
