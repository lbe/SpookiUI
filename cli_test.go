// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testBinary is the built spookiui binary used by the e2e tests.
var testBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "spookiui-e2e")
	if err != nil {
		panic(err)
	}
	testBinary = filepath.Join(dir, "spookiui")
	build := exec.Command("go", "build", "-o", testBinary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		os.RemoveAll(dir)
		panic("go build failed: " + err.Error())
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// stubGhosttyScript is a fake `ghostty` that serves canned output. It uses
// only shell builtins so it works with a restricted PATH.
const stubGhosttyScript = `#!/bin/sh
case "$1" in
"+show-config")
while IFS= read -r line; do printf '%s\n' "$line"; done <<'OUT'
# The font size in points.
font-size = 13
# Whether windows have decorations.
window-decoration = true
# The theme.
theme =
# Font families.
font-family =
# Key binds.
keybind =
# Background opacity.
background-opacity = 1
# The cursor style.
#
# * ` + "`block`" + ` - filled block
# * ` + "`bar`" + ` - vertical bar
cursor-style = block
# Only supported on macOS.
macos-titlebar-style = transparent
# Only supported on Linux.
gtk-single-instance = true
OUT
;;
"+list-themes")
printf '%s\n' 'Dracula (resources)' 'Solarized Dark (resources)' 'Plain Theme'
;;
"+list-fonts")
printf '%s\n' 'Menlo' '  Menlo Bold' 'Monaco' 'Menlo'
;;
"+list-actions")
printf '%s\n' 'copy_to_clipboard' 'increase_font_size' 'paste_from_clipboard'
;;
"+list-keybinds")
printf '%s\n' 'keybind = super+c=copy_to_clipboard'
;;
"+validate-config")
file=""
for a in "$@"; do
case "$a" in --config-file=*) file="${a#--config-file=}";; esac
done
if [ -n "$file" ] && [ -f "$file" ]; then
bad=0
while IFS= read -r line; do
case "$line" in bogus*) bad=1;; esac
done < "$file"
if [ "$bad" = 1 ]; then
printf '%s\n' "$file: bogus is not a valid option"
exit 1
fi
fi
exit 0
;;
esac
exit 0
`

// e2e is a hermetic environment: a stub ghostty on PATH, temp HOME/XDG dirs,
// and no access to the user's real config or cache.
type e2e struct {
	t       *testing.T
	bin     string
	env     []string
	home    string
	xdgCfg  string
	xdgData string
	resDir  string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	stubDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubDir, "ghostty"),
		[]byte(stubGhosttyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &e2e{
		t:       t,
		bin:     testBinary,
		home:    t.TempDir(),
		xdgCfg:  t.TempDir(),
		xdgData: t.TempDir(),
		resDir:  t.TempDir(),
	}
	if err := os.MkdirAll(filepath.Join(e.resDir, "themes"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.env = []string{
		"PATH=" + stubDir,
		"HOME=" + e.home,
		"XDG_CONFIG_HOME=" + e.xdgCfg,
		"XDG_CACHE_HOME=" + t.TempDir(),
		"XDG_DATA_HOME=" + e.xdgData,
		"GHOSTTY_RESOURCES_DIR=" + e.resDir,
		"SPOOKIUI_NO_UPDATE_CHECK=1",
		"SHELL=/bin/sh",
	}
	return e
}

// run executes the binary and returns stdout, stderr, exit code.
func (e *e2e) run(args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			e.t.Fatalf("failed to run %v: %v", args, err)
		}
	}
	return outBuf.String(), errBuf.String(), code
}

func (e *e2e) configPath() string {
	return filepath.Join(e.xdgCfg, "ghostty", "config")
}

func (e *e2e) readConfig() string {
	e.t.Helper()
	data, err := os.ReadFile(e.configPath())
	if err != nil {
		return ""
	}
	return string(data)
}

func (e *e2e) writeConfig(text string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.configPath()), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath(), []byte(text), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func TestE2EVersion(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("version", "--no-check")
	if code != 0 || out != "SpookiUI v2.8.0\n" {
		t.Errorf("version: code=%d out=%q", code, out)
	}
	out, _, code = e.run("-V")
	if code != 0 || out != "SpookiUI v2.8.0\n" {
		t.Errorf("-V: code=%d out=%q", code, out)
	}
	out, _, code = e.run("--version")
	if code != 0 || out != "SpookiUI v2.8.0\n" {
		t.Errorf("--version: code=%d out=%q", code, out)
	}
}

func TestE2EHelp(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("--help")
	if code != 0 {
		t.Fatalf("--help: code=%d", code)
	}
	for _, sub := range []string{"list", "get", "doc", "set", "version", "update",
		"reset", "reload", "validate", "themes", "fonts", "path", "profile",
		"doctor", "fix-ssh"} {
		if !strings.Contains(out, sub) {
			t.Errorf("--help missing subcommand %q", sub)
		}
	}
}

func TestE2EList(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("list")
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	for _, want := range []string{"== Font ==", "font-size"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// Platform-specific options are filtered by the runtime OS.
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(out, "macos-titlebar-style") {
			t.Error("macos option should be listed on darwin")
		}
		if strings.Contains(out, "gtk-single-instance") {
			t.Error("linux-only option should be hidden on darwin without --all")
		}
	case "linux":
		if !strings.Contains(out, "gtk-single-instance") {
			t.Error("linux option should be listed on linux")
		}
		if strings.Contains(out, "macos-titlebar-style") {
			t.Error("macos-only option should be hidden on linux without --all")
		}
	}
	out, _, _ = e.run("list", "--all")
	for _, want := range []string{"gtk-single-instance", "macos-titlebar-style"} {
		if !strings.Contains(out, want) {
			t.Errorf("--all should show %s", want)
		}
	}
	// Category filter.
	out, _, code = e.run("list", "Font")
	if code != 0 || !strings.Contains(out, "font-size") || strings.Contains(out, "== Window ==") {
		t.Errorf("list Font: code=%d\n%s", code, out)
	}
}

func TestE2EGetAndDoc(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("get", "font-size")
	if code != 0 || out != "13\n" {
		t.Errorf("get font-size: code=%d out=%q", code, out)
	}
	_, errOut, code := e.run("get", "bogus-key")
	if code != 2 || !strings.Contains(errOut, "unknown option: bogus-key") {
		t.Errorf("get bogus-key: code=%d err=%q", code, errOut)
	}
	out, _, code = e.run("doc", "cursor-style")
	if code != 0 {
		t.Fatalf("doc: code=%d", code)
	}
	for _, want := range []string{"cursor-style  (type: enum)", "default: block",
		"choices: block, bar", "The cursor style."} {
		if !strings.Contains(out, want) {
			t.Errorf("doc output missing %q:\n%s", want, out)
		}
	}
	_, errOut, code = e.run("doc", "bogus-key")
	if code != 2 || !strings.Contains(errOut, "unknown option: bogus-key") {
		t.Errorf("doc bogus-key: code=%d err=%q", code, errOut)
	}
}

func TestE2ESetAndReset(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("set", "font-size", "14", "--no-reload")
	if code != 0 || out != "font-size set · saved (no reload)\n" {
		t.Errorf("set --no-reload: code=%d out=%q", code, out)
	}
	cfg := e.readConfig()
	if !strings.Contains(cfg, "font-size = 14") || !strings.Contains(cfg, managedHeader) {
		t.Errorf("config after set:\n%s", cfg)
	}

	// With reload: still exit 0, message mentions reload.
	out, _, code = e.run("set", "font-size", "15")
	if code != 0 || !strings.HasPrefix(out, "font-size set · saved · reload:") {
		t.Errorf("set: code=%d out=%q", code, out)
	}

	// Unknown key -> exit 2, config untouched.
	_, errOut, code := e.run("set", "bogus-key", "x")
	if code != 2 || !strings.Contains(errOut, "unknown option: bogus-key") {
		t.Errorf("set bogus-key: code=%d err=%q", code, errOut)
	}

	// List option takes multiple values.
	out, _, code = e.run("set", "font-family", "Menlo", "Monaco", "--no-reload")
	if code != 0 {
		t.Errorf("set list: code=%d out=%q", code, out)
	}
	cfg = e.readConfig()
	if !strings.Contains(cfg, "font-family = Menlo") || !strings.Contains(cfg, "font-family = Monaco") {
		t.Errorf("config after list set:\n%s", cfg)
	}

	// reset requires --yes.
	_, errOut, code = e.run("reset")
	if code != 1 || !strings.Contains(errOut, "Re-run with --yes to proceed.") {
		t.Errorf("reset without --yes: code=%d err=%q", code, errOut)
	}
	out, _, code = e.run("reset", "--yes")
	if code != 0 || !strings.Contains(out, "restored Ghostty defaults") {
		t.Errorf("reset --yes: code=%d out=%q", code, out)
	}
	cfg = e.readConfig()
	if !strings.Contains(cfg, "# Configuration reset to Ghostty defaults by SpookiUI.") {
		t.Errorf("config after reset:\n%s", cfg)
	}
}

func TestE2EValidate(t *testing.T) {
	e := newE2E(t)
	e.writeConfig("font-size = 13\n")
	out, _, code := e.run("validate")
	if code != 0 || out != "config is valid\n" {
		t.Errorf("validate: code=%d out=%q", code, out)
	}
	e.writeConfig("bogus = 1\n")
	_, errOut, code := e.run("validate")
	if code != 1 || !strings.Contains(errOut, "bogus is not a valid option") {
		t.Errorf("validate invalid: code=%d err=%q", code, errOut)
	}
}

func TestE2EThemesFontsPath(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("themes")
	if code != 0 || out != "Dracula\nSolarized Dark\nPlain Theme\n" {
		t.Errorf("themes: code=%d out=%q", code, out)
	}
	out, _, code = e.run("fonts")
	if code != 0 || out != "Menlo\nMonaco\n" {
		t.Errorf("fonts: code=%d out=%q", code, out)
	}
	out, _, code = e.run("path")
	if code != 0 || out != e.configPath()+"\n" {
		t.Errorf("path: code=%d out=%q want %q", code, out, e.configPath())
	}
}

func TestE2EProfiles(t *testing.T) {
	e := newE2E(t)
	e.writeConfig("font-size = 10\n")
	out, _, code := e.run("profile", "save", "dark")
	if code != 0 || !strings.Contains(out, "saved profile 'dark'") {
		t.Errorf("save dark: code=%d out=%q", code, out)
	}
	e.writeConfig("font-size = 22\n")
	if _, _, saveCode := e.run("profile", "save", "light"); saveCode != 0 {
		t.Errorf("save light: code=%d", saveCode)
	}
	out, _, code = e.run("profile", "list")
	if code != 0 || out != "dark\nlight\n" {
		t.Errorf("list: code=%d out=%q", code, out)
	}
	out, _, code = e.run("profile", "show", "dark")
	if code != 0 || out != "font-size = 10\n" {
		t.Errorf("show: code=%d out=%q", code, out)
	}
	// Current config matches light, so toggle switches to dark.
	out, _, code = e.run("profile", "toggle")
	if code != 0 || !strings.HasPrefix(out, "loaded profile 'dark'") {
		t.Errorf("toggle: code=%d out=%q", code, out)
	}
	if cfg := e.readConfig(); cfg != "font-size = 10\n" {
		t.Errorf("config after toggle:\n%s", cfg)
	}
	out, _, code = e.run("profile", "delete", "dark")
	if code != 0 || !strings.Contains(out, "deleted profile 'dark'") {
		t.Errorf("delete: code=%d out=%q", code, out)
	}
	_, errOut, code := e.run("profile", "delete", "dark")
	if code != 1 || !strings.Contains(errOut, "no profile named 'dark'") {
		t.Errorf("delete missing: code=%d err=%q", code, errOut)
	}
	// Missing name and bad action are usage errors.
	_, _, code = e.run("profile", "save")
	if code != 2 {
		t.Errorf("save without name: code=%d", code)
	}
	_, _, code = e.run("profile", "frobnicate")
	if code != 2 {
		t.Errorf("bad action: code=%d", code)
	}
}

func TestE2EDoctor(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("doctor")
	if code != 0 || !strings.Contains(out, "no issues found") ||
		!strings.Contains(out, "0 error(s), 0 warning(s)") {
		t.Errorf("doctor healthy: code=%d out=%q", code, out)
	}
	e.writeConfig("not-in-schema = 1\n")
	out, _, code = e.run("doctor")
	if code != 0 || !strings.Contains(out, "unknown option `not-in-schema`") ||
		!strings.Contains(out, "0 error(s), 1 warning(s)") {
		t.Errorf("doctor warn: code=%d out=%q", code, out)
	}
}

func TestE2EFixSSH(t *testing.T) {
	e := newE2E(t)
	out, _, code := e.run("fix-ssh", "--explain")
	if code != 0 || !strings.Contains(out, "Error opening terminal: xterm-ghostty") {
		t.Errorf("explain: code=%d out=%q", code, out)
	}
	_, errOut, code := e.run("fix-ssh", "--check")
	if code != 1 || !strings.Contains(errOut, "ssh alias not found") {
		t.Errorf("check: code=%d err=%q", code, errOut)
	}
	out, _, code = e.run("fix-ssh")
	if code != 0 || !strings.Contains(out, "added the ssh alias to ~/.zshrc") {
		t.Errorf("apply: code=%d out=%q", code, out)
	}
	data, err := os.ReadFile(filepath.Join(e.home, ".zshrc"))
	if err != nil || !strings.Contains(string(data), sshAliasLine) {
		t.Errorf("rc after apply: %q err=%v", data, err)
	}
	out, _, code = e.run("fix-ssh", "--check")
	if code != 0 || !strings.Contains(out, "ssh alias present in ~/.zshrc") {
		t.Errorf("check after apply: code=%d out=%q", code, out)
	}
}

func TestE2EUnknownCommandAndArgs(t *testing.T) {
	e := newE2E(t)
	_, errOut, code := e.run("frobnicate")
	if code != 2 || errOut == "" {
		t.Errorf("unknown command: code=%d err=%q", code, errOut)
	}
	_, _, code = e.run("get")
	if code != 2 {
		t.Errorf("get without key: code=%d", code)
	}
	_, _, code = e.run("set", "font-size")
	if code != 2 {
		t.Errorf("set without value: code=%d", code)
	}
	_, _, code = e.run("list", "--bogus-flag")
	if code != 2 {
		t.Errorf("unknown flag: code=%d", code)
	}
}

func TestE2ENoSubcommandRunsTUIStub(t *testing.T) {
	e := newE2E(t)
	_, errOut, code := e.run()
	if code != 1 || !strings.Contains(errOut, "Refusing to launch the TUI without a terminal") {
		t.Errorf("no subcommand: code=%d err=%q", code, errOut)
	}
}
