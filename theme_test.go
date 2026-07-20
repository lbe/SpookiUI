package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSplitKeybind(t *testing.T) {
	cases := []struct {
		in, trig, act string
	}{
		{"super+c=copy_to_clipboard", "super+c", "copy_to_clipboard"},
		{"super+==increase_font_size", "super+=", "increase_font_size"},
		{"noequals", "noequals", ""},
		{"=", "", ""},
		{"a=b=c", "a=b", "c"},
	}
	for _, c := range cases {
		trig, act := splitKeybind(c.in)
		if trig != c.trig || act != c.act {
			t.Errorf("splitKeybind(%q) = (%q, %q), want (%q, %q)", c.in, trig, act, c.trig, c.act)
		}
	}
}

func TestThemeVariantName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Dracula", "Dracula"},
		{"light:A,dark:B", "B"},
		{"dark:B,light:A", "B"},
		{"A,B", "A"},
		{"catppuccin-mocha", "catppuccin-mocha"},
		{"light: A, dark: B", "B"},
		{"x:", "x:"},
		{":", ":"},
		{"  Dracula  ", "Dracula"},
	}
	for _, c := range cases {
		if got := themeVariantName(c.in); got != c.want {
			t.Errorf("themeVariantName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// setupThemeDirs points theme search paths at a temp resources dir with the
// given theme files. Returns the themes dir.
func setupThemeDirs(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	res := t.TempDir()
	themes := filepath.Join(res, "themes")
	if err := os.MkdirAll(themes, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(themes, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GHOSTTY_RESOURCES_DIR", res)
	origCache := themeColorCache
	themeColorCache = map[string]*themeColors{}
	t.Cleanup(func() { themeColorCache = origCache })
	return themes
}

func TestThemeSearchDirsAndFind(t *testing.T) {
	themes := setupThemeDirs(t, map[string]string{"Dracula": "background = #000\n"})
	dirs := themeSearchDirs()
	if dirs[len(dirs)-1] != themes {
		t.Errorf("last search dir = %q, want %q", dirs[len(dirs)-1], themes)
	}
	if got := findThemeFile("Dracula"); got != filepath.Join(themes, "Dracula") {
		t.Errorf("findThemeFile = %q", got)
	}
	if got := findThemeFile("Nope"); got != "" {
		t.Errorf("findThemeFile(Nope) = %q, want \"\"", got)
	}
	if got := findThemeFile("  "); got != "" {
		t.Errorf("findThemeFile(blank) = %q, want \"\"", got)
	}
}

func TestParseThemeColors(t *testing.T) {
	setupThemeDirs(t, map[string]string{
		"Dracula": "# comment\npalette = 0=#282a36\npalette = 1=#ff5555\n" +
			"background = #282a36\nforeground = #f8f8f2\ncursor-color = #eeeeee\nbogus line\npalette = xx=#bad\n",
	})
	got := parseThemeColors("Dracula")
	if got == nil {
		t.Fatal("parseThemeColors returned nil")
	}
	wantPalette := map[int]string{0: "#282a36", 1: "#ff5555"}
	if !reflect.DeepEqual(got.Palette, wantPalette) {
		t.Errorf("palette = %v, want %v", got.Palette, wantPalette)
	}
	if got.Background != "#282a36" || got.Foreground != "#f8f8f2" || got.Cursor != "#eeeeee" {
		t.Errorf("colors = %+v", got)
	}
	// Missing theme -> nil.
	if got := parseThemeColors("NoSuchTheme"); got != nil {
		t.Errorf("missing theme = %+v, want nil", got)
	}
}

func TestParseHex(t *testing.T) {
	cases := []struct {
		in      string
		r, g, b int
		ok      bool
	}{
		{"#ff0080", 255, 0, 128, true},
		{"ff0080", 255, 0, 128, true},
		{" #AABBCC ", 170, 187, 204, true},
		{"#fff", 0, 0, 0, false},
		{"gg0000", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"#ff00800", 0, 0, 0, false},
	}
	for _, c := range cases {
		r, g, b, ok := parseHex(c.in)
		if ok != c.ok || (ok && (r != c.r || g != c.g || b != c.b)) {
			t.Errorf("parseHex(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.in, r, g, b, ok, c.r, c.g, c.b, c.ok)
		}
	}
}

// Oracle values from Python rgb_to_256.
func TestRGBTo256(t *testing.T) {
	cases := []struct {
		r, g, b, want int
	}{
		{0, 0, 0, 16},
		{255, 255, 255, 231},
		{255, 0, 0, 196},
		{128, 128, 128, 244},
		{26, 188, 156, 37},
		{200, 200, 200, 251},
		{35, 35, 35, 234},
		{90, 90, 90, 240},
		{255, 128, 0, 208},
	}
	for _, c := range cases {
		if got := rgbTo256(c.r, c.g, c.b); got != c.want {
			t.Errorf("rgbTo256(%d,%d,%d) = %d, want %d", c.r, c.g, c.b, got, c.want)
		}
	}
}
