// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"reflect"
	"testing"
)

func TestEnumValuesFromDoc(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			"simple bullets",
			"The style.\n* `block` - filled block\n* `bar` - vertical bar\n* `underline` - under",
			[]string{"block", "bar", "underline"},
		},
		{
			"packed values with wrapped continuation",
			"* `blueprint`, `chalkboard`,\n  `microchip`, `paper` - the icon style",
			[]string{"blueprint", "chalkboard", "microchip", "paper"},
		},
		{
			"em dash separator",
			"* `a` — first\n* `b` — second",
			[]string{"a", "b"},
		},
		{
			"backticks after the description are ignored",
			"* `a` - uses `xyz` internally",
			[]string{"a"},
		},
		{
			"prose backticks not in bullets are ignored",
			"Set this to `something` for fun.",
			nil,
		},
		{
			"continuation stops at non-backtick line",
			"* `a`,\n`b`,\nplain text\n`c`",
			[]string{"a", "b"},
		},
		{"no bullets", "just documentation", nil},
		{"empty", "", nil},
	}
	for _, c := range cases {
		got := enumValuesFromDoc(c.doc)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: enumValuesFromDoc() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name           string
		opt            Option
		wantKind       string
		wantIsList     bool
		wantValues     []string
		wantReloadNote string
	}{
		{"bool", Option{Name: "window-decoration", Default: "true"}, "bool", false, []string{"true", "false"}, ""},
		{"theme", Option{Name: "theme", Default: ""}, "theme", false, nil, ""},
		{"palette", Option{Name: "palette", Default: ""}, "palette", true, nil, ""},
		{"keybind", Option{Name: "keybind", Default: ""}, "keybind", true, nil, ""},
		{"font-family", Option{Name: "font-family", Default: ""}, "font", true, nil, ""},
		{"font-family-bold", Option{Name: "font-family-bold", Default: ""}, "font", true, nil, ""},
		{
			"enum from doc",
			Option{Name: "cursor-style", Default: "block",
				Doc: "* `block` - b\n* `bar` - v\n* `underline` - u\n* `block_hollow` - h"},
			"enum", false, []string{"block", "bar", "underline", "block_hollow"}, "",
		},
		{
			"enum dedupes",
			Option{Name: "x-enum", Default: "a", Doc: "* `a` - 1\n* `b` - 2\n* `a` - 3"},
			"enum", false, []string{"a", "b"}, "",
		},
		{
			"enum requires default in candidates",
			Option{Name: "x-enum2", Default: "zzz", Doc: "* `a` - 1\n* `b` - 2"},
			"text", false, nil, "",
		},
		{"int", Option{Name: "font-size", Default: "13"}, "int", false, nil, ""},
		{"negative int", Option{Name: "x-int", Default: "-42"}, "int", false, nil, ""},
		{"float", Option{Name: "x-float", Default: "0.5"}, "float", false, nil, ""},
		{"opacity named int default is float", Option{Name: "background-opacity", Default: "1"}, "float", false, nil, ""},
		{"minimum-contrast int default is float", Option{Name: "minimum-contrast", Default: "1"}, "float", false, nil, ""},
		{"list", Option{Name: "env", Default: "", IsList: true}, "list", true, nil, ""},
		{"color by suffix", Option{Name: "cursor-color", Default: ""}, "color", false, nil, ""},
		{"color by hint", Option{Name: "background", Default: ""}, "color", false, nil, ""},
		{"color by selection prefix", Option{Name: "selection-background", Default: ""}, "color", false, nil, ""},
		{"plain text", Option{Name: "window-title", Default: ""}, "text", false, nil, ""},
		{
			"reload note full restart",
			Option{Name: "x", Default: "", Doc: "This cannot be reloaded at runtime."},
			"text", false, nil, "needs full Ghostty restart",
		},
		{
			"reload note new windows",
			Option{Name: "x", Default: "", Doc: "This only applies to new windows."},
			"text", false, nil, "applies to new windows only",
		},
		{
			"reload note new surfaces",
			Option{Name: "x", Default: "", Doc: "This will not affect existing surfaces."},
			"text", false, nil, "affects new surfaces only",
		},
	}
	for _, c := range cases {
		opt := c.opt
		if opt.Kind == "" {
			opt.Kind = "text"
		}
		classify(&opt)
		if opt.Kind != c.wantKind {
			t.Errorf("%s: kind = %q, want %q", c.name, opt.Kind, c.wantKind)
		}
		if opt.IsList != c.wantIsList {
			t.Errorf("%s: isList = %v, want %v", c.name, opt.IsList, c.wantIsList)
		}
		if !reflect.DeepEqual(opt.Values, c.wantValues) {
			t.Errorf("%s: values = %v, want %v", c.name, opt.Values, c.wantValues)
		}
		if opt.ReloadNote != c.wantReloadNote {
			t.Errorf("%s: reloadNote = %q, want %q", c.name, opt.ReloadNote, c.wantReloadNote)
		}
	}
}

func TestCategorize(t *testing.T) {
	cases := []struct{ name, want string }{
		{"theme", "Colors & Theme"},
		{"palette", "Colors & Theme"},
		{"background-opacity", "Colors & Theme"},
		{"cursor-color", "Colors & Theme"},
		{"selection-background", "Colors & Theme"},
		{"search-highlight-color", "Colors & Theme"},
		{"cursor-style", "Cursor"},
		{"cursor-invert-fg-bg", "Cursor"},
		{"font-size", "Font"},
		{"font-family", "Font"},
		{"adjust-cell-height", "Spacing & Metrics"},
		{"window-padding-x", "Spacing & Metrics"},
		{"grapheme-width-method", "Spacing & Metrics"},
		{"window-decoration", "Window"},
		{"window-width", "Window"},
		{"maximize", "Window"},
		{"confirm-close-surface", "Window"},
		{"mouse-scroll-multiplier", "Mouse"},
		{"focus-follows-mouse", "Mouse"},
		{"link", "Mouse"},
		{"clipboard-read", "Clipboard & Selection"},
		{"copy-on-select", "Clipboard & Selection"},
		{"quick-terminal-position", "Quick Terminal"},
		{"shell-integration", "Shell & Commands"},
		{"command", "Shell & Commands"},
		{"scrollback-limit", "Shell & Commands"},
		{"keybind", "Keybindings"},
		{"key-remap", "Keybindings"},
		{"macos-option-as-alt", "macOS"},
		{"gtk-single-instance", "Linux / GTK"},
		{"x11-something", "Linux / GTK"},
		{"linux-cgroup", "Linux / GTK"},
		{"some-unknown-thing", "Advanced"},
	}
	for _, c := range cases {
		if got := categorize(c.name); got != c.want {
			t.Errorf("categorize(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPlatformOf(t *testing.T) {
	cases := []struct {
		name string
		opt  Option
		want string
	}{
		{"macos prefix", Option{Name: "macos-titlebar-style"}, "macos"},
		{"gtk prefix", Option{Name: "gtk-tabs-location"}, "linux"},
		{"x11 prefix", Option{Name: "x11-thing"}, "linux"},
		{"adw prefix", Option{Name: "adw-toolbar-style"}, "linux"},
		{"linux prefix", Option{Name: "linux-cgroup"}, "linux"},
		{"doc mac only", Option{Name: "x", Doc: "This is only supported on macOS."}, "macos"},
		{"doc linux only", Option{Name: "x", Doc: "This is only supported on Linux."}, "linux"},
		{"doc cross platform wins", Option{Name: "x", Doc: "Only supported on macOS and Linux."}, ""},
		{"doc both markers", Option{Name: "x", Doc: "Only supported on macOS. No effect on macOS?"}, ""},
		{"plain option", Option{Name: "font-size", Doc: "The size."}, ""},
	}
	for _, c := range cases {
		if got := platformOf(&c.opt); got != c.want {
			t.Errorf("%s: platformOf() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPlatformVisible(t *testing.T) {
	orig := currentPlatform
	defer func() { currentPlatform = orig }()

	currentPlatform = "macos"
	if !platformVisible(&Option{Name: "macos-x", Platform: "macos"}) {
		t.Error("macos option should be visible on macos")
	}
	if platformVisible(&Option{Name: "gtk-x", Platform: "linux"}) {
		t.Error("linux option should be hidden on macos")
	}
	if !platformVisible(&Option{Name: "font-size"}) {
		t.Error("cross-platform option should be visible")
	}
	currentPlatform = ""
	if !platformVisible(&Option{Name: "gtk-x", Platform: "linux"}) {
		t.Error("unknown current platform shows everything")
	}
}

func TestSliderRange(t *testing.T) {
	min, max, step, ok := sliderRange(&Option{Name: "minimum-contrast", Kind: "float"})
	if !ok || min != 1.0 || max != 21.0 || step != 0.5 {
		t.Errorf("minimum-contrast: got %v %v %v %v", min, max, step, ok)
	}
	_, _, _, ok = sliderRange(&Option{Name: "font-size", Kind: "int"})
	if ok {
		t.Error("font-size should not be a slider")
	}
	// Doc-driven: any 0-1 opacity option is picked up from its docs.
	_, _, _, ok = sliderRange(&Option{Name: "future-opacity", Kind: "float",
		Doc: "1 is fully opaque, 0 is fully transparent."})
	if !ok {
		t.Error("doc-driven opacity option should be a slider")
	}
	_, _, _, ok = sliderRange(&Option{Name: "other-float", Kind: "float", Doc: "Just a float."})
	if ok {
		t.Error("unrelated float should not be a slider")
	}
}

const cannedShowConfig = `# The font size in points.
font-size = 13
# Whether new windows are decorated.
window-decoration = true
# The cursor style.
#
# * ` + "`block`" + ` - filled block
# * ` + "`bar`" + ` - vertical bar
cursor-style = block
# A repeated list option.
keybind = super+c=copy_to_clipboard
keybind = super+v=paste_from_clipboard
# Only supported on macOS.
macos-titlebar-style = transparent

# Orphaned docs that belong to no key (blank line reset above is absent here,
# but this comment block is followed by a non-key line).
not a key line
background-opacity = 1
`

func TestParseSchema(t *testing.T) {
	schema := parseSchema(cannedShowConfig)

	fs := schema["font-size"]
	if fs == nil {
		t.Fatal("font-size missing")
	}
	if fs.Default != "13" || fs.Kind != "int" || fs.Doc != "The font size in points." {
		t.Errorf("font-size: %+v", fs)
	}

	kb := schema["keybind"]
	if kb == nil || !kb.IsList || kb.Kind != "keybind" {
		t.Fatalf("keybind: %+v", kb)
	}
	if !reflect.DeepEqual(kb.Defaults, []string{"super+c=copy_to_clipboard", "super+v=paste_from_clipboard"}) {
		t.Errorf("keybind defaults: %v", kb.Defaults)
	}

	cs := schema["cursor-style"]
	if cs.Kind != "enum" || !reflect.DeepEqual(cs.Values, []string{"block", "bar"}) {
		t.Errorf("cursor-style: %+v", cs)
	}

	mt := schema["macos-titlebar-style"]
	if mt.Platform != "macos" || mt.Category != "macOS" {
		t.Errorf("macos-titlebar-style: %+v", mt)
	}

	// "not a key line" resets the doc buffer, so background-opacity gets no doc.
	bo := schema["background-opacity"]
	if bo == nil {
		t.Fatal("background-opacity missing")
	}
	if bo.Doc != "" {
		t.Errorf("background-opacity doc = %q, want empty", bo.Doc)
	}
	if bo.Kind != "float" {
		t.Errorf("background-opacity kind = %q, want float", bo.Kind)
	}

	wd := schema["window-decoration"]
	if wd.Kind != "bool" || !reflect.DeepEqual(wd.Values, []string{"true", "false"}) {
		t.Errorf("window-decoration: %+v", wd)
	}
}
