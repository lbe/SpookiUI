// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// newTestConfigFile writes text to a temp config and loads it.
func newTestConfigFile(t *testing.T, text string) *ConfigFile {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewConfigFile(p)
}

func TestConfigFileReload(t *testing.T) {
	cf := NewConfigFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if len(cf.Lines) != 0 {
		t.Errorf("missing file: lines = %v, want empty", cf.Lines)
	}
	cf = newTestConfigFile(t, "a = 1\nb = 2\n")
	if !reflect.DeepEqual(cf.Lines, []string{"a = 1", "b = 2", ""}) {
		t.Errorf("lines = %v", cf.Lines)
	}
	// An empty (but existing) file splits to one empty line, like Python.
	cf = newTestConfigFile(t, "")
	if !reflect.DeepEqual(cf.Lines, []string{""}) {
		t.Errorf("empty file: lines = %v", cf.Lines)
	}
}

func TestIndicesOf(t *testing.T) {
	cf := newTestConfigFile(t, "  indented = 5\nnoequals\n# comment = 7\nfoo = 1\nfoo=2\nfoo =\"quoted\"\n")
	if got := cf.IndicesOf("indented"); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("indented: %v", got)
	}
	if got := cf.IndicesOf("noequals"); len(got) != 0 {
		t.Errorf("noequals: %v", got)
	}
	if got := cf.IndicesOf("comment"); len(got) != 0 {
		t.Errorf("comment: %v", got)
	}
	if got := cf.IndicesOf("foo"); !reflect.DeepEqual(got, []int{3, 4, 5}) {
		t.Errorf("foo: %v", got)
	}
}

func TestGetValuesAndGetValue(t *testing.T) {
	cf := newTestConfigFile(t, "f = \"a b\"\nf = plain\ng =   spaced   \n")
	if got := cf.GetValues("f"); !reflect.DeepEqual(got, []string{"a b", "plain"}) {
		t.Errorf("GetValues(f) = %v", got)
	}
	if got, ok := cf.GetValue("f"); !ok || got != "plain" {
		t.Errorf("GetValue(f) = %q, %v; want plain, true", got, ok)
	}
	if _, ok := cf.GetValue("missing"); ok {
		t.Error("GetValue(missing) should report not-found")
	}
	if got, ok := cf.GetValue("g"); !ok || got != "spaced" {
		t.Errorf("GetValue(g) = %q, %v; want spaced, true", got, ok)
	}
}

func TestSetScalarOnEmptyFile(t *testing.T) {
	cf := newTestConfigFile(t, "")
	cf.SetScalar("theme", "Dracula")
	want := "\n" + managedHeader + "\ntheme = Dracula"
	if cf.Render() != want {
		t.Errorf("render = %q, want %q", cf.Render(), want)
	}
}

func TestSetScalarNewKeyAppendsManaged(t *testing.T) {
	cf := newTestConfigFile(t, "font-size = 13")
	cf.SetScalar("theme", "Dracula")
	want := "font-size = 13\n\n" + managedHeader + "\ntheme = Dracula"
	if cf.Render() != want {
		t.Errorf("render = %q, want %q", cf.Render(), want)
	}
}

func TestSetScalarReplacesLastAndSupersedesDuplicates(t *testing.T) {
	cf := newTestConfigFile(t, "font-size = 12\nfont-size = 13\n")
	cf.SetScalar("font-size", "14")
	want := "# font-size = 12  # (superseded)\nfont-size = 14\n"
	if cf.Render() != want {
		t.Errorf("render = %q, want %q", cf.Render(), want)
	}
}

func TestSetScalarQuoting(t *testing.T) {
	// Existing double-quoted value keeps its quote style.
	cf := newTestConfigFile(t, "font-family = \"JetBrains Mono\"\n")
	cf.SetScalar("font-family", "Iosevka Term")
	if cf.Render() != "font-family = \"Iosevka Term\"\n" {
		t.Errorf("quote-preserved: %q", cf.Render())
	}

	// Unquoted existing value + value with a space -> auto-quoted.
	cf = newTestConfigFile(t, "font-family = JetBrains\n")
	cf.SetScalar("font-family", "Iosevka Term")
	if cf.Render() != "font-family = \"Iosevka Term\"\n" {
		t.Errorf("auto-quote: %q", cf.Render())
	}

	// Embedded quotes are escaped.
	cf = newTestConfigFile(t, "")
	cf.SetScalar("window-title", `say "hi"`)
	want := "\n" + managedHeader + "\nwindow-title = \"say \\\"hi\\\"\""
	if cf.Render() != want {
		t.Errorf("escape: render = %q, want %q", cf.Render(), want)
	}

	// Plain value stays unquoted.
	cf = newTestConfigFile(t, "")
	cf.SetScalar("font-size", "14")
	want = "\n" + managedHeader + "\nfont-size = 14"
	if cf.Render() != want {
		t.Errorf("plain: render = %q, want %q", cf.Render(), want)
	}
}

func TestSetList(t *testing.T) {
	// Replaces all existing entries at the position of the first.
	cf := newTestConfigFile(t, "keybind = a=b\nfoo = 1\nkeybind = c=d\n")
	cf.SetList("keybind", []string{"super+shift+x=do thing", "alt+q=quit"})
	want := "keybind = \"super+shift+x=do thing\"\nkeybind = alt+q=quit\nfoo = 1\n"
	if cf.Render() != want {
		t.Errorf("replace: render = %q, want %q", cf.Render(), want)
	}

	// New key appends under the managed header.
	cf = newTestConfigFile(t, "foo = 1\n")
	cf.SetList("env", []string{"A=1", "B=2"})
	want = "foo = 1\n\n" + managedHeader + "\nenv = A=1\nenv = B=2"
	if cf.Render() != want {
		t.Errorf("append: render = %q, want %q", cf.Render(), want)
	}

	// Empty values on an existing key removes all entries.
	cf = newTestConfigFile(t, "env = A=1\nenv = B=2\nkeep = yes\n")
	cf.SetList("env", nil)
	want = "keep = yes\n"
	if cf.Render() != want {
		t.Errorf("clear: render = %q, want %q", cf.Render(), want)
	}

	// Empty values on a missing key is a no-op (no header created).
	cf = newTestConfigFile(t, "keep = yes\n")
	cf.SetList("env", nil)
	want = "keep = yes\n"
	if cf.Render() != want {
		t.Errorf("no-op: render = %q, want %q", cf.Render(), want)
	}
}

func TestUnset(t *testing.T) {
	cf := newTestConfigFile(t, "a = 1\nb = 2\na = 3\n")
	cf.Unset("a")
	want := "# a = 1  # (removed)\nb = 2\n# a = 3  # (removed)\n"
	if cf.Render() != want {
		t.Errorf("render = %q, want %q", cf.Render(), want)
	}
}

func TestAppendManagedLegacyHeader(t *testing.T) {
	legacy := "# ─────────── added by GhostlyConfig ───────────"
	cf := newTestConfigFile(t, "x = 1\n"+legacy+"\ny = 2\n")
	cf.SetScalar("z", "3")
	want := "x = 1\n" + legacy + "\ny = 2\n\nz = 3"
	if cf.Render() != want {
		t.Errorf("render = %q, want %q", cf.Render(), want)
	}
}

func TestWriteTrailingNewlineAndMkdir(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "dir", "config")
	cf := NewConfigFile(p)
	cf.Lines = []string{"a = 1"}
	if err := cf.Write(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a = 1\n" {
		t.Errorf("file = %q, want %q", data, "a = 1\n")
	}
}

func TestBackup(t *testing.T) {
	cf := newTestConfigFile(t, "a = 1\n")
	dst1 := cf.Backup()
	if dst1 == "" {
		t.Fatal("Backup returned empty path")
	}
	wantName := cf.Path + ".spookiui." + time.Now().Format("20060102") + ".bak"
	if dst1 != wantName {
		t.Errorf("backup = %q, want %q", dst1, wantName)
	}
	data, err := os.ReadFile(dst1)
	if err != nil || string(data) != "a = 1\n" {
		t.Errorf("backup contents = %q, err %v", data, err)
	}
	// Second call the same day keeps the first backup.
	if err := os.WriteFile(cf.Path, []byte("a = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst2 := cf.Backup()
	if dst2 != dst1 {
		t.Errorf("second backup = %q, want same as first %q", dst2, dst1)
	}
	data, _ = os.ReadFile(dst1)
	if string(data) != "a = 1\n" {
		t.Errorf("backup was overwritten: %q", data)
	}
	// No file -> no backup.
	cf2 := NewConfigFile(filepath.Join(t.TempDir(), "missing"))
	if got := cf2.Backup(); got != "" {
		t.Errorf("missing file backup = %q, want \"\"", got)
	}
}

func TestUnquote(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"a b"`, "a b"},
		{`plain`, "plain"},
		{`  "spaced"  `, "spaced"},
		{`"say \"hi\""`, `say "hi"`},
		{`"`, `"`},             // too short to be quoted
		{`""`, ``},             // empty quoted string
		{`"single`, `"single`}, // unbalanced
	}
	for _, c := range cases {
		if got := unquote(c.in); got != c.want {
			t.Errorf("unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatLine(t *testing.T) {
	cases := []struct {
		name, value string
		quote       rune
		want        string
	}{
		{"a", "1", 0, "a = 1"},
		{"a", "x y", 0, `a = "x y"`},
		{"a", "x\ty", 0, `a = "x	y"`},
		{"a", "xy", '"', `a = "xy"`},
		{"a", `q"q`, 0, `a = q"q`}, // no whitespace -> no quoting
		{"a", `q "q`, 0, `a = "q \"q"`},
	}
	for _, c := range cases {
		if got := formatLine(c.name, c.value, c.quote); got != c.want {
			t.Errorf("formatLine(%q, %q, %q) = %q, want %q", c.name, c.value, c.quote, got, c.want)
		}
	}
}
