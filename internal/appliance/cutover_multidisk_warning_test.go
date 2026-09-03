package appliance

import (
	"os"
	"strings"
	"testing"
)

// F-14 (2c): measured live, a two-disk source's disks are captured ~54s
// apart while the source keeps running (findings.md), and the only way to
// guarantee every disk reflects the same instant is to power the source off
// before completing the cutover. These tests check that guidance is present
// (and proportionate — gated on multi-disk) in the three places an operator
// might read it: the console's cutover dialog, the served how-to guide, and
// CONSOLE.md.

// The console's cutover-step dialog (startMig) must warn about multi-disk
// capture skew and name powering the source off as the only guarantee, but
// must not show it unconditionally (single-disk migrations have no skew).
func TestCutoverDialogWarnsAboutMultiDiskSkew(t *testing.T) {
	fn := extractJSFunc(t, "async function startMig(id,btn){")
	for _, want := range []string{"nDisks>1", "power", "instant"} {
		if !strings.Contains(fn, want) {
			t.Errorf("startMig() missing %q — expected multi-disk cutover guidance gated on nDisks>1", want)
		}
	}
}

// The served how-to guide's #cutover section must carry the same guidance.
func TestDocsiteCutoverSectionWarnsAboutMultiDiskSkew(t *testing.T) {
	i := strings.Index(docsHTML, `id="cutover"`)
	if i < 0 {
		t.Fatal("docsite: #cutover section not found")
	}
	end := strings.Index(docsHTML[i:], "</section>")
	if end < 0 {
		t.Fatal("docsite: #cutover section has no closing tag")
	}
	section := docsHTML[i : i+end]
	for _, want := range []string{"apart", "power", "instant"} {
		if !strings.Contains(strings.ToLower(section), want) {
			t.Errorf("docsite #cutover section missing %q — expected multi-disk capture-skew guidance", want)
		}
	}
}

// CONSOLE.md's cutover documentation must carry the same guidance.
func TestConsoleMDWarnsAboutMultiDiskSkew(t *testing.T) {
	b, err := os.ReadFile(consoleMDPath(t))
	if err != nil {
		t.Fatalf("read CONSOLE.md: %v", err)
	}
	body := strings.ToLower(string(b))
	i := strings.Index(body, "cut over the instance")
	if i < 0 {
		t.Fatal("CONSOLE.md: cutover section not found")
	}
	section := body[i:]
	if end := strings.Index(section, "\n## "); end > 0 {
		section = section[:end]
	}
	for _, want := range []string{"apart", "power", "instant"} {
		if !strings.Contains(section, want) {
			t.Errorf("CONSOLE.md cutover section missing %q — expected multi-disk capture-skew guidance", want)
		}
	}
}

// consoleMDPath locates CONSOLE.md relative to this test file's package,
// which lives at internal/appliance — the doc is two levels up, at the repo
// root.
func consoleMDPath(t *testing.T) string {
	t.Helper()
	for _, candidate := range []string{"../../CONSOLE.md", "CONSOLE.md"} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatal("CONSOLE.md not found relative to the test working directory")
	return ""
}
