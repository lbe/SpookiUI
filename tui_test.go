// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- TUI test scaffolding ----

// stubTUIGhostty stubs the ghostty CLI (and reload path) with listings for
// the theme/font/action pickers.
func stubTUIGhostty(t *testing.T) {
	t.Helper()
	origPath, origRun := ghosttyPath, runCmd
	ghosttyPath = "/stub/ghostty"
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if len(args) >= 2 && args[0] == ghosttyPath {
			switch args[1] {
			case "+list-themes":
				return cmdResult{stdout: "Dracula (resources)\nSolarized Dark (resources)\nPlain Theme\n"}, nil
			case "+list-fonts":
				return cmdResult{stdout: "Menlo\nMonaco\nJetBrainsMono Nerd Font\n"}, nil
			case "+list-actions":
				return cmdResult{stdout: "copy_to_clipboard\nincrease_font_size\npaste_from_clipboard\n"}, nil
			}
		}
		return cmdResult{}, nil
	}
	t.Cleanup(func() { ghosttyPath, runCmd = origPath, origRun })
}

// tuiTestSchema builds the schema directly (bypassing +show-config parsing)
// so tests control kinds and categories exactly.
func tuiTestSchema() map[string]*Option {
	return map[string]*Option{
		"theme": {
			Name: "theme", Kind: "theme", Category: "Colors & Theme",
			Doc: "The theme to use.",
		},
		"background": {
			Name: "background", Kind: "color", Default: "#000000",
			Category: "Colors & Theme", Doc: "The background color.",
		},
		"background-opacity": {
			Name: "background-opacity", Kind: "float", Default: "1",
			Category: "Colors & Theme",
			Doc:      "The opacity of the background. 1 is fully opaque, 0 is fully transparent.",
		},
		"palette": {
			Name: "palette", Kind: "palette", IsList: true,
			Category: "Colors & Theme", Doc: "The 256-color palette.",
		},
		"font-size": {
			Name: "font-size", Kind: "int", Default: "13",
			Category: "Font", Doc: "The font size in points.",
		},
		"font-family": {
			Name: "font-family", Kind: "font", IsList: true,
			Category: "Font", Doc: "Font families to use.",
		},
		"cursor-style": {
			Name: "cursor-style", Kind: "enum", Default: "block",
			Values: []string{"block", "bar"}, Category: "Cursor",
			Doc: "The cursor style.",
		},
		"window-decoration": {
			Name: "window-decoration", Kind: "bool", Default: "true",
			Values: []string{"true", "false"}, Category: "Window",
			Doc: "Whether windows have decorations.",
		},
		"keybind": {
			Name: "keybind", Kind: "keybind", IsList: true,
			Category: "Keybindings", Doc: "Key binds.",
		},
	}
}

// newTUISession builds a Session with the TUI schema against a temp config.
func newTUISession(t *testing.T, configText string) *Session {
	t.Helper()
	stubTUIGhostty(t)
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "1")
	if configText != "" {
		if err := os.MkdirAll(filepath.Join(cfgDir, "ghostty"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "ghostty", "config"), []byte(configText), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := NewConfigFile(configPath())
	return &Session{
		Schema:       tuiTestSchema(),
		Cfg:          cfg,
		OriginalText: cfg.Render(),
		AutoApply:    false,
	}
}

// newTUIApp builds an App wired to in-memory I/O: scripted keys go into
// a.keys, frames accumulate in a.fb (flush is a no-op).
func newTUIApp(t *testing.T, configText string) *App {
	t.Helper()
	sess := newTUISession(t, configText)
	a := newApp(sess, false, false)
	a.rows, a.cols = 24, 80
	a.keys = make(chan KeyEvent, 64)
	a.resize = make(chan struct{}, 1)
	a.sizeFn = func() (int, int) { return a.rows, a.cols }
	a.flushFn = func(*frameBuffer) {}
	return a
}

// send queues key events for the app's next reads.
func (a *App) send(keys ...KeyEvent) {
	for _, k := range keys {
		a.keys <- k
	}
}

func runes(s string) []KeyEvent {
	var out []KeyEvent
	for _, r := range s {
		out = append(out, KeyEvent{Kind: keyRune, Rune: r})
	}
	return out
}

// screenFromFrame renders a frameBuffer into a rune grid (one string per
// row), interpreting cursor positioning and ignoring SGR attributes.
func screenFromFrame(t *testing.T, fb *frameBuffer) []string {
	t.Helper()
	grid := make([][]rune, fb.rows)
	for i := range grid {
		grid[i] = make([]rune, fb.cols)
		for j := range grid[i] {
			grid[i][j] = ' '
		}
	}
	clear := func() {
		for i := range grid {
			for j := range grid[i] {
				grid[i][j] = ' '
			}
		}
	}
	data := fb.render()
	x, y := 0, 0
	for i := 0; i < len(data); {
		if data[i] == 0x1b && i+1 < len(data) && data[i+1] == '[' {
			j := i + 2
			for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
				j++
			}
			if j >= len(data) {
				break
			}
			seq, final := string(data[i+2:j]), data[j]
			switch final {
			case 'H':
				var r, c int
				if _, err := fmt.Sscanf(seq, "%d;%d", &r, &c); err == nil {
					y, x = r-1, c-1
				}
			case 'J':
				clear()
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if y >= 0 && y < fb.rows && x >= 0 && x < fb.cols {
			grid[y][x] = r
		}
		x++
		i += size
	}
	lines := make([]string, fb.rows)
	for i := range grid {
		lines[i] = strings.TrimRight(string(grid[i]), " ")
	}
	return lines
}

// screenText joins the frame's rows for substring assertions.
func screenText(t *testing.T, fb *frameBuffer) string {
	t.Helper()
	return strings.Join(screenFromFrame(t, fb), "\n")
}

// ---- Tests ----

func TestTUIInitialDraw(t *testing.T) {
	a := newTUIApp(t, "")
	a.cols = 110 // wide enough for the full footer hints (Python clips at w too)
	a.draw()
	screen := screenText(t, a.fb)
	for _, want := range []string{
		"SpookiUI · live Ghostty configurator",
		"AUTO-APPLY:OFF",
		"manual",
		"Colors & Theme",
		"Font",
		"Cursor",
		"Window",
		"Keybindings",
		"⚙ Utils",
		"background-opacity", // option pane: first category's options
		"│",                  // pane separators
		"q quit",             // footer hints
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("initial draw missing %q\nscreen:\n%s", want, screen)
		}
	}
	// Header is on row 0, footer hints on the last row.
	lines := screenFromFrame(t, a.fb)
	if !strings.Contains(lines[0], "SpookiUI") {
		t.Errorf("header row: %q", lines[0])
	}
	if !strings.Contains(lines[23], "↑↓ category") {
		t.Errorf("footer row: %q", lines[23])
	}
	// Detail pane shows the first option of the first category.
	if !strings.Contains(screen, "type: color") || !strings.Contains(screen, "changed: no (default)") {
		t.Errorf("detail pane missing\nscreen:\n%s", screen)
	}
}

func TestTUICategoryNavAndFocus(t *testing.T) {
	a := newTUIApp(t, "")
	a.draw()
	// Down moves to the next category and resets option state.
	if !a.handleKey(KeyEvent{Kind: keyDown}) {
		t.Fatal("handleKey returned quit")
	}
	if a.catIdx != 1 || a.categories[a.catIdx] != "Font" {
		t.Fatalf("catIdx=%d cat=%q", a.catIdx, a.categories[a.catIdx])
	}
	// Up wraps around to the last category (Utils).
	a.handleKey(KeyEvent{Kind: keyUp})
	a.handleKey(KeyEvent{Kind: keyUp})
	if a.curCat() != utilsCategory {
		t.Fatalf("expected wrap to Utils, got %q", a.curCat())
	}
	a.draw()
	if screen := screenText(t, a.fb); !strings.Contains(screen, "Fix SSH") {
		t.Errorf("utils pane missing Fix SSH\nscreen:\n%s", screen)
	}
	// Back to Font (j wraps Utils → first category, then Font); Enter focuses
	// the option pane.
	a.handleKey(KeyEvent{Kind: keyRune, Rune: 'j'})
	a.handleKey(KeyEvent{Kind: keyRune, Rune: 'j'})
	if a.curCat() != "Font" {
		t.Fatalf("cat=%q", a.curCat())
	}
	a.handleKey(KeyEvent{Kind: keyEnter})
	if a.focus != "opts" {
		t.Fatalf("focus=%q", a.focus)
	}
	// Option nav: down to the second Font option.
	a.handleKey(KeyEvent{Kind: keyDown})
	if a.optIdx != 1 {
		t.Fatalf("optIdx=%d", a.optIdx)
	}
	a.draw()
	if screen := screenText(t, a.fb); !strings.Contains(screen, "font-size") {
		t.Errorf("detail should show font-size\nscreen:\n%s", screen)
	}
	// Left goes back to categories.
	a.handleKey(KeyEvent{Kind: keyLeft})
	if a.focus != "cats" {
		t.Fatalf("focus=%q", a.focus)
	}
	// Tab toggles focus.
	a.handleKey(KeyEvent{Kind: keyTab})
	if a.focus != "opts" {
		t.Fatalf("after tab focus=%q", a.focus)
	}
}

func TestTUISearchFilterAndCancel(t *testing.T) {
	a := newTUIApp(t, "")
	a.draw()
	a.handleKey(KeyEvent{Kind: keyRune, Rune: '/'})
	if !a.searchMode {
		t.Fatal("not in search mode")
	}
	if len(a.searchResults) != len(tuiTestSchema()) {
		t.Fatalf("initial results=%d", len(a.searchResults))
	}
	for _, k := range runes("font") {
		a.handleKey(k)
	}
	if got := a.searchResults; len(got) != 2 || got[0] != "font-family" || got[1] != "font-size" {
		t.Fatalf("results=%v", got)
	}
	if !strings.Contains(a.status, "search: font   (2 matches)") {
		t.Errorf("status=%q", a.status)
	}
	// Backspace refines the query.
	a.handleKey(KeyEvent{Kind: keyBackspace})
	if a.search != "fon" {
		t.Fatalf("search=%q", a.search)
	}
	// Esc exits search into the option pane (the category cursor is
	// untouched — Python keeps cat_idx).
	a.handleKey(KeyEvent{Kind: keyEscape})
	if a.searchMode || a.focus != "opts" {
		t.Fatalf("searchMode=%v focus=%q", a.searchMode, a.focus)
	}
	a.draw()
	screen := screenText(t, a.fb)
	if !strings.Contains(screen, "background-opacity") {
		t.Errorf("after search cancel, category options should show\nscreen:\n%s", screen)
	}
	if strings.Contains(screen, "exit search") {
		t.Errorf("search footer hints should be gone\nscreen:\n%s", screen)
	}
}

func TestTUISearchAcceptEditsOption(t *testing.T) {
	a := newTUIApp(t, "")
	a.draw()
	a.handleKey(KeyEvent{Kind: keyRune, Rune: '/'})
	for _, k := range runes("cursor") {
		a.handleKey(k)
	}
	// Enter starts editing the first (only) result: cursor-style is an enum,
	// so the picker opens. Feed picker keys: down, Enter to pick "bar".
	a.send(KeyEvent{Kind: keyDown}, KeyEvent{Kind: keyEnter})
	a.handleKey(KeyEvent{Kind: keyEnter})
	if got := a.sess.Effective("cursor-style"); got != "bar" {
		t.Fatalf("cursor-style=%q", got)
	}
	if !a.sess.Dirty {
		t.Error("expected staged change (dirty)")
	}
}

func TestTUIBoolToggle(t *testing.T) {
	a := newTUIApp(t, "")
	a.editBool(a.sess.Schema["window-decoration"])
	if got := a.sess.Effective("window-decoration"); got != "false" {
		t.Fatalf("value=%q", got)
	}
	if !a.sess.Dirty {
		t.Error("expected dirty after staged toggle")
	}
	if !strings.Contains(a.status, "window-decoration = false (staged)") {
		t.Errorf("status=%q", a.status)
	}
	a.editBool(a.sess.Schema["window-decoration"])
	if got := a.sess.Effective("window-decoration"); got != "true" {
		t.Fatalf("value=%q", got)
	}
}

func TestTUIEnumPickerSelect(t *testing.T) {
	a := newTUIApp(t, "")
	// Picker starts on the current value ("block"); down + enter picks "bar".
	a.send(KeyEvent{Kind: keyDown}, KeyEvent{Kind: keyEnter})
	a.editEnum(a.sess.Schema["cursor-style"])
	if got := a.sess.Effective("cursor-style"); got != "bar" {
		t.Fatalf("value=%q", got)
	}
	if !strings.Contains(a.status, "cursor-style = bar (staged)") {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUIEnumPickerCancelRestores(t *testing.T) {
	a := newTUIApp(t, "cursor-style = bar\n")
	a.send(KeyEvent{Kind: keyDown}, KeyEvent{Kind: keyEscape})
	a.editEnum(a.sess.Schema["cursor-style"])
	if got := a.sess.Effective("cursor-style"); got != "bar" {
		t.Fatalf("value=%q", got)
	}
	if a.status != "cancelled" {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUINumberEditStepAndType(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["font-size"]
	// Step up twice with '+', then commit with Enter.
	a.send(KeyEvent{Kind: keyRune, Rune: '+'}, KeyEvent{Kind: keyRune, Rune: '+'},
		KeyEvent{Kind: keyEnter})
	a.editNumber(opt)
	if got := a.sess.Effective("font-size"); got != "15" {
		t.Fatalf("after stepping value=%q", got)
	}
	// Typed entry: backspace both digits, type 22, Enter.
	a.send(KeyEvent{Kind: keyBackspace}, KeyEvent{Kind: keyBackspace})
	a.send(runes("22")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.editNumber(opt)
	if got := a.sess.Effective("font-size"); got != "22" {
		t.Fatalf("after typing value=%q", got)
	}
	// Arrow stepping works too, and Esc cancels without committing.
	a.send(KeyEvent{Kind: keyUp}, KeyEvent{Kind: keyUp}, KeyEvent{Kind: keyEscape})
	a.editNumber(opt)
	if got := a.sess.Effective("font-size"); got != "22" {
		t.Fatalf("after esc value=%q", got)
	}
	if a.status != "cancelled" {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUINumberEditIgnoresNonNumeric(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["font-size"]
	// Letters are not accepted in the number editor (Python: only 0-9 . -).
	a.send(runes("ab")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.editNumber(opt)
	if got := a.sess.Effective("font-size"); got != "13" {
		t.Fatalf("value=%q", got)
	}
}

func TestTUISliderStepAndCancelRestores(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["background-opacity"]
	if _, _, _, ok := sliderRange(opt); !ok {
		t.Fatal("expected background-opacity to have a slider range")
	}
	// Start at 1.0 (max). Left steps down 0.05; Enter commits.
	a.send(KeyEvent{Kind: keyLeft}, KeyEvent{Kind: keyLeft}, KeyEvent{Kind: keyEnter})
	a.editSlider(opt, 0, 1, 0.05)
	if got := a.sess.Effective("background-opacity"); got != "0.9" {
		t.Fatalf("value=%q", got)
	}
	// Stepping then Esc restores the snapshot.
	a.send(KeyEvent{Kind: keyLeft}, KeyEvent{Kind: keyLeft}, KeyEvent{Kind: keyEscape})
	a.editSlider(opt, 0, 1, 0.05)
	if got := a.sess.Effective("background-opacity"); got != "0.9" {
		t.Fatalf("after esc value=%q", got)
	}
	if a.status != "cancelled" {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUISliderDraw(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["background-opacity"]
	a.drawSlider(opt, 0, 1, 0.5, true, 0.5)
	screen := screenText(t, a.fb)
	for _, want := range []string{"set · background-opacity", "━", "●", "0.5", "Enter apply · Esc cancel"} {
		if !strings.Contains(screen, want) {
			t.Errorf("slider frame missing %q\nscreen:\n%s", want, screen)
		}
	}
}

func TestTUITextLineEditor(t *testing.T) {
	a := newTUIApp(t, "")
	// Backspace removes, runes append, Enter accepts.
	a.send(KeyEvent{Kind: keyBackspace})
	a.send(runes("y")...)
	a.send(KeyEvent{Kind: keyEnter})
	buf, ok := a.lineEditor("x = ", "hello", "hint", nil)
	if !ok || buf != "helly" {
		t.Fatalf("buf=%q ok=%v", buf, ok)
	}
	// Esc cancels.
	a.send(KeyEvent{Kind: keyEscape})
	if buf2, ok2 := a.lineEditor("x = ", "abc", "hint", nil); ok2 || buf2 != "" {
		t.Fatalf("esc: buf=%q ok=%v", buf2, ok2)
	}
	// Non-ASCII runes are ignored (Python only accepts 32 <= ch < 127).
	a.send(KeyEvent{Kind: keyUTF8, Rune: 'é'})
	a.send(runes("a")...)
	a.send(KeyEvent{Kind: keyEnter})
	buf, ok = a.lineEditor("x = ", "", "hint", nil)
	if !ok || buf != "a" {
		t.Fatalf("non-ascii: buf=%q ok=%v", buf, ok)
	}
}

func TestTUIEditTextColorLivePreview(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["background"]
	// The color kind commits live on every keystroke when auto-apply is on.
	a.sess.AutoApply = true
	// Clear the current value, then type the new one.
	for i := 0; i < 7; i++ {
		a.send(KeyEvent{Kind: keyBackspace})
	}
	a.send(runes("#112233")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.editText(opt)
	if got := a.sess.Effective("background"); got != "#112233" {
		t.Fatalf("value=%q", got)
	}
	if a.sess.Dirty {
		t.Error("live commit should leave dirty=false")
	}
	data, err := os.ReadFile(a.sess.Cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "background = #112233") {
		t.Errorf("config not written live:\n%s", data)
	}
}

func TestTUIListEditorAddDelete(t *testing.T) {
	a := newTUIApp(t, "")
	opt := a.sess.Schema["palette"]
	// a → type entry → Enter; a → second entry → Enter; d deletes the second;
	// Enter saves the list.
	a.send(KeyEvent{Kind: keyRune, Rune: 'a'})
	a.send(runes("4=#89b4fa")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.send(KeyEvent{Kind: keyRune, Rune: 'a'})
	a.send(runes("1=#ff0000")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.send(KeyEvent{Kind: keyRune, Rune: 'd'})
	a.send(KeyEvent{Kind: keyEnter})
	a.editList(opt)
	vals := a.sess.EffectiveList("palette")
	if len(vals) != 1 || vals[0] != "4=#89b4fa" {
		t.Fatalf("values=%v", vals)
	}
	if !strings.Contains(a.status, "palette: 1 entries (staged)") {
		t.Errorf("status=%q", a.status)
	}
	// The list frame shows entries and the empty-hint is gone.
	if screen := screenText(t, a.fb); !strings.Contains(screen, "edit list · palette") {
		t.Errorf("list frame:\n%s", screen)
	}
}

func TestTUIKeybindAssembleAndParse(t *testing.T) {
	st := &keybindState{mods: map[string]bool{}}
	for _, m := range keybindMods {
		st.mods[m] = false
	}
	parseKeybindInto("cmd+shift+k=clear_screen", st)
	if !st.mods["super"] || !st.mods["shift"] || st.mods["ctrl"] || st.mods["alt"] {
		t.Errorf("mods=%v", st.mods)
	}
	if st.key != "k" || st.action != "clear_screen" || st.args != "" {
		t.Errorf("key=%q action=%q args=%q", st.key, st.action, st.args)
	}
	if got := assembleKeybind(st); got != "super+shift+k=clear_screen" {
		t.Errorf("assembled=%q", got)
	}
	// Args are appended with a colon (fresh form state, as the real form does).
	st2 := &keybindState{mods: map[string]bool{}}
	for _, m := range keybindMods {
		st2.mods[m] = false
	}
	parseKeybindInto("ctrl+equal=goto_split:1", st2)
	if got := assembleKeybind(st2); got != "ctrl+equal=goto_split:1" {
		t.Errorf("with args=%q", got)
	}
}

func TestTUIKeybindFormHappyPath(t *testing.T) {
	a := newTUIApp(t, "")
	// Row 0: Space toggles "super". Tab → row 1: type 'k'. Tab → row 2:
	// Enter opens the action picker; filter "inc" and Enter picks it.
	// Tab → row 3 (args): Enter jumps to row 4. Enter saves.
	a.send(
		KeyEvent{Kind: keyRune, Rune: ' '},
		KeyEvent{Kind: keyTab},
		KeyEvent{Kind: keyRune, Rune: 'k'},
		KeyEvent{Kind: keyTab},
		KeyEvent{Kind: keyEnter},
	)
	a.send(runes("inc")...)
	a.send(
		KeyEvent{Kind: keyEnter},
		KeyEvent{Kind: keyTab},
		KeyEvent{Kind: keyEnter},
		KeyEvent{Kind: keyEnter},
	)
	got, ok := a.editKeybindForm("")
	if !ok || got != "super+k=increase_font_size" {
		t.Fatalf("got=%q ok=%v", got, ok)
	}
}

func TestTUIKeybindFormValidationGuards(t *testing.T) {
	a := newTUIApp(t, "")
	// Jump straight to the save row with no key/action: the form reports an
	// error and refuses. Then Esc cancels.
	a.send(KeyEvent{Kind: keyUp}) // wraps to row 4
	a.send(KeyEvent{Kind: keyEnter})
	a.send(KeyEvent{Kind: keyEscape})
	if _, ok := a.editKeybindForm(""); ok {
		t.Fatal("expected cancel")
	}
	if screen := screenText(t, a.fb); !strings.Contains(screen, "pick a key first") {
		t.Errorf("expected validation error on screen\nscreen:\n%s", screen)
	}
}

func TestTUIChangesOverlay(t *testing.T) {
	a := newTUIApp(t, "")
	a.editBool(a.sess.Schema["window-decoration"])
	a.send(KeyEvent{Kind: keyRune, Rune: ' '})
	a.changesOverlay()
	screen := screenText(t, a.fb)
	for _, want := range []string{"changed from default · 1 option(s)", "window-decoration", "= false"} {
		if !strings.Contains(screen, want) {
			t.Errorf("changes overlay missing %q\nscreen:\n%s", want, screen)
		}
	}
}

func TestTUIQuitConfirmWithStagedChanges(t *testing.T) {
	// Decline the save: config file untouched, loop exits.
	a := newTUIApp(t, "")
	a.editBool(a.sess.Schema["window-decoration"])
	a.send(KeyEvent{Kind: keyRune, Rune: 'n'})
	if a.handleKey(KeyEvent{Kind: keyRune, Rune: 'q'}) {
		t.Fatal("quit should return false")
	}
	data, _ := os.ReadFile(a.sess.Cfg.Path)
	if strings.Contains(string(data), "window-decoration") {
		t.Errorf("config written despite declining:\n%s", data)
	}

	// Accept the save: Apply writes the staged change.
	a2 := newTUIApp(t, "")
	a2.editBool(a2.sess.Schema["window-decoration"])
	a2.send(KeyEvent{Kind: keyRune, Rune: 'y'})
	if a2.handleKey(KeyEvent{Kind: keyRune, Rune: 'q'}) {
		t.Fatal("quit should return false")
	}
	data, _ = os.ReadFile(a2.sess.Cfg.Path)
	if !strings.Contains(string(data), "window-decoration = false") {
		t.Errorf("config not saved on confirm:\n%s", data)
	}
}

func TestTUICtrlCQuits(t *testing.T) {
	a := newTUIApp(t, "")
	if a.handleKey(KeyEvent{Kind: keyCtrlC}) {
		t.Fatal("Ctrl-C should quit")
	}
}

func TestTUIResizeRedraw(t *testing.T) {
	a := newTUIApp(t, "")
	a.draw()
	a.sizeFn = func() (int, int) { return 30, 100 }
	a.resize <- struct{}{}
	if k := a.nextEvent(0); k != resizeKey {
		t.Fatalf("expected resizeKey, got %v", k.Kind)
	}
	if a.rows != 30 || a.cols != 100 {
		t.Fatalf("dims=%dx%d", a.rows, a.cols)
	}
	a.draw()
	if a.fb.cols != 100 || a.fb.rows != 30 {
		t.Fatalf("frame=%dx%d", a.fb.cols, a.fb.rows)
	}
	lines := screenFromFrame(t, a.fb)
	if utf8.RuneCountInString(lines[0]) > 100 {
		t.Errorf("header overflowed new width")
	}
	if !strings.Contains(lines[29], "q quit") {
		t.Errorf("footer not redrawn at new height: %q", lines[29])
	}
}

func TestTUIThemePickerPreview(t *testing.T) {
	a := newTUIApp(t, "")
	a.sess.AutoApply = true // live preview only happens with auto-apply
	done := make(chan struct{})
	go func() {
		a.editTheme(a.sess.Schema["theme"])
		close(done)
	}()
	// Wait for the debounced preview to commit the highlighted theme live
	// (poll the config file only — a.fb is owned by the picker goroutine).
	deadline := time.After(5 * time.Second)
	for {
		data, _ := os.ReadFile(a.sess.Cfg.Path)
		if strings.Contains(string(data), "theme = Dracula") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("live preview never committed")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Esc cancels: snapshot restored, config back to no theme.
	a.send(KeyEvent{Kind: keyEscape})
	<-done
	// The last frame drawn is the picker's: theme list + live-preview hint.
	screen := screenText(t, a.fb)
	for _, want := range []string{"select theme  (3)", "Dracula", "LIVE PREVIEW"} {
		if !strings.Contains(screen, want) {
			t.Errorf("picker frame missing %q\nscreen:\n%s", want, screen)
		}
	}
	data, _ := os.ReadFile(a.sess.Cfg.Path)
	if strings.Contains(string(data), "theme = Dracula") {
		t.Errorf("cancel did not restore:\n%s", data)
	}
	if a.status != "cancelled" {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUIThemePickerSelect(t *testing.T) {
	a := newTUIApp(t, "")
	a.send(runes("sol")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.editTheme(a.sess.Schema["theme"])
	if got := a.sess.Effective("theme"); got != "Solarized Dark" {
		t.Fatalf("theme=%q", got)
	}
}

func TestTUIFontPicker(t *testing.T) {
	a := newTUIApp(t, "")
	a.send(KeyEvent{Kind: keyDown}, KeyEvent{Kind: keyEnter})
	a.editFont(a.sess.Schema["font-family"])
	vals := a.sess.EffectiveList("font-family")
	if len(vals) != 1 || vals[0] != "Monaco" {
		t.Fatalf("fonts=%v", vals)
	}
}

func TestTUIResetCurrent(t *testing.T) {
	a := newTUIApp(t, "font-size = 22\n")
	// Put the Font category's font-size option under the cursor.
	a.handleKey(KeyEvent{Kind: keyDown}) // Font category
	a.handleKey(KeyEvent{Kind: keyDown}) // Cursor category... navigate directly instead
	a.catIdx = 1                         // Font
	a.focus = "opts"
	for i, n := range a.currentNames() {
		if n == "font-size" {
			a.optIdx = i
		}
	}
	a.resetCurrent()
	if a.sess.IsOverridden("font-size") {
		t.Error("expected font-size back at default")
	}
	if !strings.Contains(a.status, "reset to default") {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUIDoctorOverlay(t *testing.T) {
	a := newTUIApp(t, "bogus-option = 1\n")
	a.send(KeyEvent{Kind: keyRune, Rune: ' '}) // any key closes
	a.doctorOverlay()
	screen := screenText(t, a.fb)
	if !strings.Contains(screen, "config check") {
		t.Errorf("doctor overlay header missing\nscreen:\n%s", screen)
	}
	if !strings.Contains(screen, "bogus-option") {
		t.Errorf("expected unknown-option finding\nscreen:\n%s", screen)
	}
}

func TestTUIUtilsOverlayAndResult(t *testing.T) {
	a := newTUIApp(t, "")
	t.Setenv("SHELL", "/bin/zsh")
	// Open the utils overlay, run Fix SSH (confirm 'y'), then close the
	// result screen, then Esc out of the overlay.
	a.send(KeyEvent{Kind: keyEnter})
	a.send(KeyEvent{Kind: keyRune, Rune: 'y'}) // confirm run
	a.send(KeyEvent{Kind: keyRune, Rune: ' '}) // dismiss result
	a.send(KeyEvent{Kind: keyEscape})
	a.utilsOverlay()
	// The alias was appended to the temp HOME's rc file.
	rc, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".zshrc"))
	if err != nil {
		t.Fatalf("rc not written: %v", err)
	}
	if !strings.Contains(string(rc), "TERM=xterm-256color ssh") {
		t.Errorf("alias missing:\n%s", rc)
	}
	// Back in the overlay, the status line now reports the fix as applied.
	if screen := screenText(t, a.fb); !strings.Contains(screen, "status: applied · alias in ~/.zshrc") {
		t.Errorf("expected applied status\nscreen:\n%s", screen)
	}
}

func TestTUIHelpOverlay(t *testing.T) {
	a := newTUIApp(t, "")
	a.rows = 44 // tall enough for the full help text (Python truncates at h-1 too)
	a.send(KeyEvent{Kind: keyRune, Rune: ' '})
	a.help()
	screen := screenText(t, a.fb)
	for _, want := range []string{"help", "Navigation", "q   quit", "latest version"} {
		if !strings.Contains(screen, want) {
			t.Errorf("help missing %q\nscreen:\n%s", want, screen)
		}
	}
}

func TestTUIProfilesOverlaySaveLoad(t *testing.T) {
	a := newTUIApp(t, "font-size = 22\n")
	// Save the current config as profile 'work', then close with Esc.
	a.send(KeyEvent{Kind: keyRune, Rune: 's'})
	a.send(runes("work")...)
	a.send(KeyEvent{Kind: keyEnter})
	a.send(KeyEvent{Kind: keyEscape})
	a.profilesOverlay()
	if !strings.Contains(a.status, "saved profile 'work'") {
		t.Fatalf("status=%q", a.status)
	}
	profiles := listProfiles()
	if len(profiles) != 1 || profiles[0] != "work" {
		t.Fatalf("profiles=%v", profiles)
	}
	// Change the config, then load the profile back (confirm 'y').
	a.sess.Cfg.SetScalar("font-size", "30")
	a.send(KeyEvent{Kind: keyEnter}) // load selected
	a.send(KeyEvent{Kind: keyRune, Rune: 'y'})
	a.send(KeyEvent{Kind: keyEscape})
	a.profilesOverlay()
	if got := a.sess.Effective("font-size"); got != "22" {
		t.Fatalf("font-size=%q", got)
	}
	if !strings.Contains(a.status, "loaded profile 'work'") {
		t.Errorf("status=%q", a.status)
	}
}

func TestTUIPickerTypeToFilter(t *testing.T) {
	a := newTUIApp(t, "")
	items := []string{"alpha", "beta", "gamma", "delta"}
	// Type "ta", move down, select.
	a.send(runes("ta")...)
	a.send(KeyEvent{Kind: keyDown}, KeyEvent{Kind: keyEnter})
	got, ok := a.picker("test", items, "", nil, nil)
	if !ok || got != "delta" {
		t.Fatalf("got=%q ok=%v", got, ok)
	}
	// Filtering to nothing then Enter cancels.
	a2 := newTUIApp(t, "")
	a2.send(runes("zzz")...)
	a2.send(KeyEvent{Kind: keyEnter})
	if got, ok := a2.picker("test", items, "", nil, nil); ok || got != "" {
		t.Fatalf("empty filter: got=%q ok=%v", got, ok)
	}
}

func TestTUINextEventClosedChannelQuits(t *testing.T) {
	a := newTUIApp(t, "")
	close(a.keys)
	if k := a.nextEvent(0); k.Kind != keyCtrlC {
		t.Fatalf("kind=%v", k.Kind)
	}
}

// TestTUIFrameResetsAttrsAroundErase is the BCE regression test: a frame
// whose last write leaves SGR at pair 1 (black on cyan, as drawFooter's hint
// bar does) must not leak that background into the next frame's erase —
// Ghostty/xterm erase blank cells with the *current* background color.
func TestTUIFrameResetsAttrsAroundErase(t *testing.T) {
	a := newTUIApp(t, "")
	a.fb = newFrameBuffer(a.rows, a.cols)
	a.fb.addstr(a.rows-1, 0, "footer", textAttr{pair: 1})
	frame1 := a.fb.render()
	if !bytes.HasSuffix(frame1, []byte("\x1b[0m")) {
		t.Errorf("frame with pair-1 last write must end with SGR reset, got %q", frame1)
	}
	a.newScreen()
	frame2 := a.fb.render()
	if !bytes.HasPrefix(frame2, []byte("\x1b[0m\x1b[2J")) {
		t.Errorf("new frame must reset SGR before erasing, got %q", frame2)
	}
	if !bytes.HasSuffix(frame2, []byte("\x1b[0m")) {
		t.Errorf("new frame must end with SGR reset, got %q", frame2)
	}
}

func TestTUIAutoApplyToggleAndSave(t *testing.T) {
	a := newTUIApp(t, "")
	a.handleKey(KeyEvent{Kind: keyRune, Rune: 'a'})
	if !a.sess.AutoApply {
		t.Fatal("auto-apply should be on")
	}
	if !strings.Contains(a.status, "auto-apply ON") {
		t.Errorf("status=%q", a.status)
	}
	a.draw()
	if screen := screenText(t, a.fb); !strings.Contains(screen, "AUTO-APPLY:ON") {
		t.Errorf("header missing AUTO-APPLY:ON\nscreen:\n%s", screen)
	}
	// Stage a change then save with 's'.
	a.editBool(a.sess.Schema["window-decoration"])
	if a.sess.Dirty {
		t.Fatal("live toggle should not stay dirty")
	}
	a.sess.AutoApply = false
	a.editBool(a.sess.Schema["window-decoration"])
	a.handleKey(KeyEvent{Kind: keyRune, Rune: 's'})
	if a.sess.Dirty {
		t.Error("dirty after save")
	}
	data, _ := os.ReadFile(a.sess.Cfg.Path)
	if !strings.Contains(string(data), "window-decoration = true") {
		t.Errorf("saved config:\n%s", data)
	}
}
