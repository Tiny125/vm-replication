package appliance

import (
	"regexp"
	"strings"
	"testing"
)

// themeKey is the single localStorage key both surfaces read and write.
const themeKey = "vmrepl-theme"

var tokenRe = regexp.MustCompile(`--[a-z0-9-]+\s*:`)

// tokenBlocks pulls the three token blocks out of a document: the light :root,
// the prefers-color-scheme dark block, and the [data-theme="dark"] block.
func tokenBlocks(t *testing.T, doc, name string) map[string]map[string]bool {
	t.Helper()
	blocks := map[string]string{}

	// Light: the first ":root{" that is not the attribute-selector form.
	if i := strings.Index(doc, ":root{"); i >= 0 {
		blocks["light"] = doc[i:min(i+3000, len(doc))]
	}
	if i := strings.Index(doc, "prefers-color-scheme:dark"); i >= 0 {
		blocks["media-dark"] = doc[i:min(i+3000, len(doc))]
	}
	if i := strings.Index(doc, `:root[data-theme="dark"]{`); i >= 0 {
		blocks["attr-dark"] = doc[i:min(i+3000, len(doc))]
	}
	for _, k := range []string{"light", "media-dark", "attr-dark"} {
		if blocks[k] == "" {
			t.Fatalf("%s: could not find the %q token block", name, k)
		}
	}

	out := map[string]map[string]bool{}
	for k, chunk := range blocks {
		// Stop at the end of the declaration block.
		if end := strings.Index(chunk, "\n }"); end > 0 {
			chunk = chunk[:end]
		}
		set := map[string]bool{}
		for _, m := range tokenRe.FindAllString(chunk, -1) {
			set[strings.TrimSuffix(strings.TrimSpace(m), ":")] = true
		}
		out[k] = set
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Every token defined for one theme must be defined for the others. This is how
// theming actually rots: someone adds --banner-ok-bg to the light palette, forgets
// the dark blocks, and that one element stays light-on-dark until a user reports it.
func TestThemeTokenSetsMatch(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"console", consoleHTML},
		{"docs", docsHTML},
	} {
		sets := tokenBlocks(t, tc.doc, tc.name)
		light, media, attr := sets["light"], sets["media-dark"], sets["attr-dark"]
		if len(light) == 0 {
			t.Fatalf("%s: no tokens found in the light palette", tc.name)
		}
		for name := range light {
			if !media[name] {
				t.Errorf("%s: %s is defined for light but missing from the prefers-color-scheme dark block", tc.name, name)
			}
			if !attr[name] {
				t.Errorf("%s: %s is defined for light but missing from the [data-theme=dark] block", tc.name, name)
			}
		}
		// And nothing may exist ONLY in a dark block: a colour whose only
		// definition is inside a media query has no light value to fall back to.
		for name := range media {
			if !light[name] {
				t.Errorf("%s: %s is defined only in the dark media block; every token needs a light value on :root", tc.name, name)
			}
		}
		// The two dark blocks are duplicates by design and must not drift.
		for name := range attr {
			if !media[name] {
				t.Errorf("%s: %s is in the [data-theme=dark] block but not the media block; they must stay identical", tc.name, name)
			}
		}
	}
}

// Colours must live in the token blocks, not scattered through the CSS or —
// worse — inside inline style="" strings built by the JavaScript, where they are
// invisible when reading the stylesheet and silently stay light in dark mode.
func TestNoHardcodedColoursOutsideTokens(t *testing.T) {
	// A property name is required: a bare #abc scan false-positives on DOM ids,
	// CSS selectors and documentation anchors.
	colourProp := regexp.MustCompile(`(?i)(background|background-color|color|border-color|border-top-color|border-left-color|fill|stroke)\s*:\s*#[0-9a-f]{3,8}`)

	for _, tc := range []struct{ name, doc string }{
		{"console", consoleHTML},
		{"docs", docsHTML},
	} {
		i := strings.Index(tc.doc, `:root[data-theme="dark"]{`)
		if i < 0 {
			t.Fatalf("%s: no dark token block", tc.name)
		}
		end := strings.Index(tc.doc[i:], "\n }")
		if end < 0 {
			t.Fatalf("%s: unterminated dark token block", tc.name)
		}
		rest := tc.doc[i+end:]
		if hits := colourProp.FindAllString(rest, -1); len(hits) > 0 {
			t.Errorf("%s: %d hardcoded colour(s) outside the token blocks — move them to tokens or both themes will not track: %v",
				tc.name, len(hits), hits[:min(len(hits), 8)])
		}
	}
}

// The theme has to be applied before first paint, or a dark-theme operator sees
// a white flash on every load. That means an inline, blocking read in <head> —
// and it must be guarded, because localStorage throws outright in some privacy
// configurations and would take the rest of the script with it.
func TestThemeAppliedBeforeFirstPaint(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"console", consoleHTML},
		{"docs", docsHTML},
	} {
		headEnd := strings.Index(tc.doc, "</head>")
		if headEnd < 0 {
			t.Fatalf("%s: no </head>", tc.name)
		}
		head := tc.doc[:headEnd]
		read := strings.Index(head, themeKey)
		if read < 0 {
			t.Errorf("%s: the theme is not read in <head>; it would flash the wrong theme on load", tc.name)
			continue
		}
		if style := strings.Index(head, "<style"); style >= 0 && read > style {
			t.Errorf("%s: the theme read must come before the stylesheet", tc.name)
		}
		snippet := head[max(0, read-200):read]
		if !strings.Contains(snippet, "try{") {
			t.Errorf("%s: the theme read in <head> must be wrapped in try/catch (localStorage throws in some privacy modes)", tc.name)
		}
		if !strings.Contains(head, "color-scheme") {
			t.Errorf("%s: <head> must declare color-scheme, or browser-painted UI (select popups, scrollbars, form internals) stays light in dark mode", tc.name)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// The toggle has to be reachable from the sign-in screen, which is the first
// thing an operator sees. The tab bar lives inside #app and is hidden until
// login, so a control placed there would be unreachable exactly when it is most
// likely to be wanted.
func TestThemeToggleReachableBeforeLogin(t *testing.T) {
	btn := strings.Index(consoleHTML, `id="themebtn"`)
	login := strings.Index(consoleHTML, `id="login"`)
	app := strings.Index(consoleHTML, `id="app"`)
	if btn < 0 {
		t.Fatal("no theme toggle in the console")
	}
	if login < 0 || app < 0 {
		t.Fatal("could not locate the #login / #app containers")
	}
	if btn > login || btn > app {
		t.Errorf("the theme toggle (at %d) must appear before #login (%d) and #app (%d) so it is outside both and usable at the sign-in screen", btn, login, app)
	}
}

// Console and documentation are the same origin and are linked from one another,
// so they must agree on where the preference is stored — otherwise switching to
// dark in one leaves the other light.
func TestThemeKeySharedAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"console", consoleHTML},
		{"docs", docsHTML},
	} {
		if !strings.Contains(tc.doc, themeKey) {
			t.Errorf("%s does not use the shared theme key %q", tc.name, themeKey)
		}
	}
}

// Three states, not two: someone who picks Light on a dark-mode machine needs a
// way back to following the system.
func TestThemeHasAutoState(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"console", consoleHTML},
		{"docs", docsHTML},
	} {
		for _, want := range []string{"auto:'light'", "light:'dark'", "dark:'auto'"} {
			if !strings.Contains(tc.doc, want) {
				t.Errorf("%s: theme cycle is missing %q; it needs auto -> light -> dark -> auto", tc.name, want)
			}
		}
		if !strings.Contains(tc.doc, "removeItem") {
			t.Errorf("%s: 'auto' must clear the stored preference so the OS setting applies", tc.name)
		}
	}
}

// A quick smoke check that the semantic banner classes exist, since the JS now
// emits class names instead of inline colours.
func TestBannerModifierClassesExist(t *testing.T) {
	for _, cls := range []string{".banner.ok{", ".banner.warn{", ".banner.bad{", ".banner.info{"} {
		if !strings.Contains(consoleHTML, cls) {
			t.Errorf("missing banner modifier %q — the JS emits these class names", cls)
		}
	}
	// The base .banner must be neutral: it used to be green-tinted, which made
	// "banner" secretly mean "success" and hid the meaning from every caller.
	base := strings.Index(consoleHTML, " .banner{")
	if base < 0 {
		t.Fatal("no base .banner rule")
	}
	rest := consoleHTML[base:]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatal("unterminated .banner rule")
	}
	rule := rest[:end] // just the base rule, not the modifiers that follow
	if strings.Contains(rule, "--green") {
		t.Errorf("the base .banner must be neutral, not green — otherwise \"banner\" secretly means \"success\": %s", rule)
	}
}
