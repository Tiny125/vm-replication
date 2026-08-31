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
		// method-selector.png stays dropped: it showed the old three-method
		// picker, and the console now offers local-disk boot only.
		"console-overview.png", "login.png", "new-migration.png",
		"settings-token.png", "migration-card.png",
		"source-helper.png", "source-check.png",
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
	for _, bad := range []string{"nope.png", "../console.go", "a/b.png", "method-selector.png"} {
		rr := httptest.NewRecorder()
		s.handleDocsImage(rr, httptest.NewRequest("GET", "/documentation/img/"+bad, nil))
		if rr.Code == 200 {
			t.Errorf("img %q must not be served", bad)
		}
	}
}

// The install step must document the one-command installer (bootstrap.sh),
// which is now the supported path, and must not mention the old default port
// (8080 — the console now defaults to 443, see enroll.go's consoleBaseURL).
// A raw `git clone` step may still appear as a secondary "build from source"
// note, but must not be the primary instruction any more.
func TestDocsInstallStepUsesBootstrap(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDocs(rr, httptest.NewRequest("GET", "/documentation", nil))
	body := rr.Body.String()

	for _, want := range []string{
		"bootstrap.sh",
		"curl -fsSL https://raw.githubusercontent.com/Tiny125/vm-replication/main/scripts/bootstrap.sh",
		"VMREPL_REF",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("install step should mention %q (the one-command installer)", want)
		}
	}
	if strings.Contains(body, "8080") {
		t.Error("documentation still references the old default port 8080 — the console now defaults to 443")
	}
}

// The console links to the documentation so operators can find the guide.
func TestConsoleLinksDocumentation(t *testing.T) {
	if !strings.Contains(consoleHTML, `href="/documentation"`) {
		t.Error("console should link to /documentation")
	}
}

// The guide must tell users which operating systems have actually been
// migrated end to end, so they can tell "we tested this" from "the code has a
// branch for it". Every version listed here corresponds to a full migration
// run — created, replicated, cut over, booted, and byte-verified — not to a
// code path that ought to work.
func TestDocsSiteListsSupportedOS(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	s.handleDocs(rr, httptest.NewRequest("GET", "/documentation", nil))
	body := rr.Body.String()

	if !strings.Contains(body, `id="supported-os"`) {
		t.Fatal("the guide needs a Supported OS section users can link to")
	}
	if !strings.Contains(body, `href="#supported-os"`) {
		t.Error("Supported OS must be reachable from the sidebar, not just by scrolling")
	}
	// Every version verified in the August 2026 campaign.
	for _, v := range []string{"20.04", "22.04", "24.04", "25.10", "26.04"} {
		if !strings.Contains(body, "Ubuntu "+v) {
			t.Errorf("Supported OS must list Ubuntu %s — it was verified end to end", v)
		}
	}
	// It must be explicit that this is a tested list, not a compatibility claim,
	// otherwise readers will assume anything absent is unsupported.
	for _, want := range []string{"tested", "x86_64"} {
		if !strings.Contains(body, want) {
			t.Errorf("the section should mention %q so the scope of the claim is clear", want)
		}
	}
}
