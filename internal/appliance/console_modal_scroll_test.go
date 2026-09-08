package appliance

import (
	"strings"
	"testing"
)

// A real user could not reach the bottom of the cutover dialog (the form
// fields and the confirm checkbox) on a live AWS -> Linode migration and had
// to zoom the browser out to finish. Every dialog goes through uiDialog
// (~17 call sites), so the fix lives there: wrap the body + field inputs +
// checkbox(es) in ONE scroll container, leaving the title and the action
// buttons pinned outside it as flex:none siblings.
//
// These tests pin the DOM/CSS shape so the bug can't silently come back:
// only fixing .modal-body's overflow (leaving the field divs / .modal-check
// labels — its SIBLINGS, not children — outside the scroll region) would
// reproduce the exact reported symptom.

// The overlay must render ONE scroll wrapper around the body + fields +
// checkbox(es), with the title and action row OUTSIDE it (pinned).
func TestUiDialogWrapsScrollableContentInOneContainer(t *testing.T) {
	fn := extractJSFunc(t, "function uiDialog(opts){")
	if !strings.Contains(fn, "modal-scroll") {
		t.Fatal("uiDialog must introduce a .modal-scroll wrapper around body+fields+checks")
	}
	// h3 (title), modal-scroll (body+fields+checks), and modal-actions
	// (buttons) must appear in that order in the built markup, with fields,
	// check and checks all landing INSIDE modal-scroll (between its opening
	// and modal-actions) rather than modal-scroll wrapping modal-body alone —
	// that's the part that reproduces the exact bug: the field divs and
	// .modal-check labels are siblings of .modal-body, so scrolling
	// modal-body alone would leave them unreachable.
	// Match the literal markup fragments (not the explanatory comment above
	// them, which mentions these class names too and would throw off a plain
	// substring search).
	h3Idx := strings.Index(fn, "'<h3>'")
	scrollIdx := strings.Index(fn, `<div class="modal-scroll">`)
	bodyIdx := strings.Index(fn, `<div class="modal-body">`)
	fieldsIdx := strings.Index(fn, "+fields+check+checks")
	actionsIdx := strings.Index(fn, `'<div class="modal-actions">'`)
	if h3Idx < 0 || scrollIdx < 0 || bodyIdx < 0 || fieldsIdx < 0 || actionsIdx < 0 {
		t.Fatalf("expected to find <h3>, modal-scroll, modal-body, fields+check+checks and modal-actions markers in uiDialog; got h3Idx=%d scrollIdx=%d bodyIdx=%d fieldsIdx=%d actionsIdx=%d", h3Idx, scrollIdx, bodyIdx, fieldsIdx, actionsIdx)
	}
	if !(h3Idx < scrollIdx && scrollIdx < bodyIdx && bodyIdx < fieldsIdx && fieldsIdx < actionsIdx) {
		t.Errorf("expected order <h3> ... modal-scroll ... modal-body ... fields+check+checks ... modal-actions, got h3Idx=%d scrollIdx=%d bodyIdx=%d fieldsIdx=%d actionsIdx=%d", h3Idx, scrollIdx, bodyIdx, fieldsIdx, actionsIdx)
	}
}

// The modal must be bounded to the viewport and become a scrolling flex
// column, with the scroll child explicitly allowed to shrink
// (min-height:0) — a flex item's default min-height:auto refuses to shrink
// below its content, which makes overflow-y:auto a silent no-op and
// reproduces the bug exactly.
func TestModalCSSIsBoundedAndScrollable(t *testing.T) {
	for _, want := range []string{
		".modal-scroll", "overflow-y:auto", "min-height:0",
	} {
		if !strings.Contains(consoleHTML, want) {
			t.Errorf("console CSS missing %q — needed so the dialog scrolls instead of overflowing the viewport", want)
		}
	}
	// .modal itself must become a bounded flex column so its children (title,
	// scroll area, actions) can lay out with the scroll area as the only
	// growing/shrinking one.
	modalCSS := extractCSSRule(t, ".modal{")
	for _, want := range []string{"display:flex", "flex-direction:column", "max-height"} {
		if !strings.Contains(modalCSS, want) {
			t.Errorf(".modal CSS rule missing %q; rule body: %s", want, modalCSS)
		}
	}
	// iOS Safari resolves position:fixed against the LARGE viewport, so a
	// max-height expressed purely in vh can exceed the visible area. A dvh
	// value (with a plain vh/% fallback first, for browsers without dvh
	// support) must also be present.
	if !strings.Contains(consoleHTML, "dvh") {
		t.Error("console CSS should include a dvh-based max-height fallback for iOS Safari's large-viewport position:fixed behavior")
	}
}

// The cutover dialog (startMig) is the tallest dialog in the console — it
// must use the wide modal so there's more room before scrolling even kicks
// in.
func TestCutoverDialogIsWide(t *testing.T) {
	fn := extractJSFunc(t, "async function startMig(id,btn){")
	if !strings.Contains(fn, "wide:true") {
		t.Error("startMig's cutover dialog must pass wide:true — it's the tallest dialog in the console and didn't opt into the wide modal")
	}
}

// extractCSSRule returns the first `{...}` block for a selector prefix like
// ".modal{" found in consoleHTML. Skips the ".modal-*" descendant selectors
// (".modal-overlay{", ".modal-body{", etc.) by requiring the character right
// after the prefix's "{" match — the prefix itself already includes the "{".
func extractCSSRule(t *testing.T, selectorPrefix string) string {
	t.Helper()
	// Search for selectorPrefix preceded by a non-identifier char (start of
	// line/rule), so ".modal{" doesn't match inside ".modal-body{".
	idx := -1
	from := 0
	for {
		i := strings.Index(consoleHTML[from:], selectorPrefix)
		if i < 0 {
			break
		}
		abs := from + i
		if abs == 0 || consoleHTML[abs-1] == ' ' || consoleHTML[abs-1] == '\n' || consoleHTML[abs-1] == '}' {
			idx = abs
			break
		}
		from = abs + 1
	}
	if idx < 0 {
		t.Fatalf("CSS selector %q not found", selectorPrefix)
	}
	rest := consoleHTML[idx:]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("CSS selector %q has no closing brace", selectorPrefix)
	}
	return rest[:end+1]
}
