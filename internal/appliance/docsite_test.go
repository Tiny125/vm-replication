package appliance

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The documentation site must be served at /documentation WITHOUT a console
// session (it's the public how-to guide reached at https://<ip>:<port>/documentation),
// and must cover the full console journey: install, sign-in, the Linode API
// token, and creating a migration with each of the three methods.
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
		"Create a migration", "File transfer", "Volume boot", "Disk boot",
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
	// No icons: the guide must not use emoji-style icons.
	for _, banned := range []string{"⚡", "🚀", "📘", "💡"} {
		if strings.Contains(body, banned) {
			t.Errorf("documentation must not use icons/emoji (found %q)", banned)
		}
	}
}

// Screenshots are embedded in the binary and served under /documentation/img/.
func TestDocsImagesServed(t *testing.T) {
	s := &Server{}
	for _, name := range []string{
		"console-overview.png", "login.png", "new-migration.png",
		"settings-token.png", "method-selector.png", "migration-card.png",
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
	for _, bad := range []string{"nope.png", "../console.go", "a/b.png"} {
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
