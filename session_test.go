package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testSchemaOutput = `# The font size in points.
font-size = 13
# Decorated windows.
window-decoration = true
# Theme.
theme =
# Fonts.
font-family =
# Binds.
keybind =
`

// stubGhostty makes the package talk to a fake ghostty through runCmd.
func stubGhostty(t *testing.T, showConfig string) {
	t.Helper()
	origPath, origRun := ghosttyPath, runCmd
	ghosttyPath = "/stub/ghostty"
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if len(args) >= 2 && args[0] == ghosttyPath {
			switch args[1] {
			case "+show-config":
				return cmdResult{stdout: showConfig}, nil
			case "+validate-config":
				return cmdResult{}, nil
			}
		}
		if args[0] == "osascript" {
			return cmdResult{}, nil
		}
		return cmdResult{}, nil
	}
	t.Cleanup(func() { ghosttyPath, runCmd = origPath, origRun })
}

// newTestSession builds a Session against a temp config, optionally with
// initial config contents.
func newTestSession(t *testing.T, configText string) *Session {
	t.Helper()
	stubGhostty(t, testSchemaOutput)
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
	sess, err := NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sess.AutoApply = false
	return sess
}

func TestSessionEffective(t *testing.T) {
	sess := newTestSession(t, "font-size = 14\n")
	if got := sess.Effective("font-size"); got != "14" {
		t.Errorf("Effective(font-size) = %q, want 14", got)
	}
	if got := sess.Effective("window-decoration"); got != "true" {
		t.Errorf("Effective(window-decoration) = %q, want default true", got)
	}
	if got := sess.Effective("not-in-schema"); got != "" {
		t.Errorf("Effective(unknown) = %q, want \"\"", got)
	}
	// List: user values beat defaults.
	sess2 := newTestSession(t, "keybind = super+x=quit\n")
	if got := sess2.EffectiveList("keybind"); !reflect.DeepEqual(got, []string{"super+x=quit"}) {
		t.Errorf("EffectiveList(keybind) = %v", got)
	}
	if got := sess.EffectiveList("keybind"); len(got) != 0 {
		t.Errorf("EffectiveList(keybind) default = %v, want empty", got)
	}
}

func TestSessionIsOverridden(t *testing.T) {
	sess := newTestSession(t, "font-size = 14\nwindow-decoration = true\nunknown-opt = x\n")
	if !sess.IsOverridden("font-size") {
		t.Error("font-size 14 vs default 13 should be overridden")
	}
	if sess.IsOverridden("window-decoration") {
		t.Error("window-decoration = default should not be overridden")
	}
	if !sess.IsOverridden("unknown-opt") {
		t.Error("key present but not in schema counts as overridden")
	}
	if sess.IsOverridden("theme") {
		t.Error("absent key is not overridden")
	}
}

func TestSessionStageApplyAndBackup(t *testing.T) {
	sess := newTestSession(t, "font-size = 13\n")
	sess.StageScalar("font-size", "14")
	if !sess.Dirty {
		t.Error("staging should mark the session dirty")
	}
	ok, msg := sess.Apply()
	if !ok {
		t.Fatalf("Apply failed: %s", msg)
	}
	if msg != "saved (auto-apply off — press 's'/reload to apply live)" {
		t.Errorf("msg = %q", msg)
	}
	if sess.Dirty {
		t.Error("apply should clear dirty")
	}
	data, err := os.ReadFile(sess.Cfg.Path)
	if err != nil || !strings.Contains(string(data), "font-size = 14") {
		t.Errorf("config after apply = %q, err %v", data, err)
	}
	if sess.BackupPath == "" || !fileExists(sess.BackupPath) {
		t.Errorf("backup not created: %q", sess.BackupPath)
	}
}

func TestSessionApplyAutoReload(t *testing.T) {
	sess := newTestSession(t, "")
	sess.AutoApply = true
	sess.StageScalar("font-size", "15")
	ok, msg := sess.Apply()
	if !ok {
		t.Fatalf("Apply failed: %s", msg)
	}
	if !strings.HasPrefix(msg, "saved") {
		t.Errorf("msg = %q", msg)
	}
}

func TestSessionRevertAll(t *testing.T) {
	sess := newTestSession(t, "font-size = 13\n")
	sess.StageScalar("font-size", "99")
	sess.StageScalar("theme", "Dracula")
	ok, msg := sess.RevertAll()
	if !ok || msg != "reverted to session start" {
		t.Errorf("RevertAll = %v, %q", ok, msg)
	}
	data, _ := os.ReadFile(sess.Cfg.Path)
	if string(data) != "font-size = 13\n" {
		t.Errorf("config after revert = %q", data)
	}
	if sess.Dirty {
		t.Error("revert should clear dirty")
	}
}

func TestSessionRestoreDefaults(t *testing.T) {
	sess := newTestSession(t, "font-size = 99\ntheme = Dracula\n")
	ok, msg := sess.RestoreDefaults()
	if !ok {
		t.Fatalf("RestoreDefaults failed: %s", msg)
	}
	if msg != "restored Ghostty defaults" {
		t.Errorf("msg = %q", msg)
	}
	data, _ := os.ReadFile(sess.Cfg.Path)
	want := managedHeader + "\n# Configuration reset to Ghostty defaults by SpookiUI.\n"
	if string(data) != want {
		t.Errorf("config = %q, want %q", data, want)
	}
	if sess.BackupPath == "" || !fileExists(sess.BackupPath) {
		t.Error("restore should keep a dated backup")
	}
}

func TestSessionOverridesSorted(t *testing.T) {
	sess := newTestSession(t, "theme = Dracula\nfont-size = 20\nkeybind = super+x=quit\nkeybind = super+y=reset\n")
	got := sess.Overrides()
	want := []override{
		{"font-size", "20"},
		{"keybind", "super+x=quit, super+y=reset"},
		{"theme", "Dracula"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Overrides() = %v, want %v", got, want)
	}
}

func TestSessionProfiles(t *testing.T) {
	sess := newTestSession(t, "font-size = 21\n")

	if ok, msg := sess.SaveProfile("bad name!"); ok || !strings.Contains(msg, "invalid name") {
		t.Errorf("invalid name: %v %q", ok, msg)
	}
	if ok, msg := sess.SaveProfile("dark"); !ok {
		t.Fatalf("save dark: %s", msg)
	}
	if got := listProfiles(); !reflect.DeepEqual(got, []string{"dark"}) {
		t.Errorf("listProfiles = %v", got)
	}

	// Load a missing profile fails.
	if ok, msg := sess.LoadProfile("nope"); ok || !strings.Contains(msg, "no profile named") {
		t.Errorf("load missing: %v %q", ok, msg)
	}
	// Delete a missing profile fails.
	if ok, msg := sess.DeleteProfile("nope"); ok || !strings.Contains(msg, "no profile named") {
		t.Errorf("delete missing: %v %q", ok, msg)
	}

	// Round trip: change config, then load the profile back.
	sess.StageScalar("font-size", "30")
	if err := sess.Cfg.Write(); err != nil {
		t.Fatal(err)
	}
	ok, msg := sess.LoadProfile("dark")
	if !ok || msg != "loaded profile 'dark'" {
		t.Errorf("load: %v %q", ok, msg)
	}
	if got := sess.Effective("font-size"); got != "21" {
		t.Errorf("after load font-size = %q, want 21", got)
	}

	if ok, msg := sess.DeleteProfile("dark"); !ok || !strings.Contains(msg, "deleted profile 'dark'") {
		t.Errorf("delete: %v %q", ok, msg)
	}
	if got := listProfiles(); len(got) != 0 {
		t.Errorf("listProfiles after delete = %v", got)
	}
}

func TestToggleLightDark(t *testing.T) {
	sess := newTestSession(t, "font-size = 10\n")
	if ok, msg := sess.ToggleLightDark(); ok || !strings.Contains(msg, "save profiles named 'light' and 'dark' first") {
		t.Fatalf("toggle without profiles: %v %q", ok, msg)
	}
	if ok, msg := sess.SaveProfile("dark"); !ok {
		t.Fatal(msg)
	}
	sess.StageScalar("font-size", "22")
	if err := sess.Cfg.Write(); err != nil {
		t.Fatal(err)
	}
	if ok, msg := sess.SaveProfile("light"); !ok {
		t.Fatal(msg)
	}
	// Current config matches "light" (22), so toggle should switch to dark.
	ok, msg := sess.ToggleLightDark()
	if !ok || msg != "loaded profile 'dark'" {
		t.Errorf("toggle: %v %q", ok, msg)
	}
	if got := sess.Effective("font-size"); got != "10" {
		t.Errorf("after toggle font-size = %q, want 10", got)
	}
	// Now current matches dark, toggle goes back to light.
	ok, msg = sess.ToggleLightDark()
	if !ok || msg != "loaded profile 'light'" {
		t.Errorf("toggle back: %v %q", ok, msg)
	}
}

func TestProfilePathAndList(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	if got := spookiuiDataDir(); got != filepath.Join(data, "spookiui") {
		t.Errorf("spookiuiDataDir = %q", got)
	}
	if got := profilePath("mine"); got != filepath.Join(data, "spookiui", "profiles", "mine") {
		t.Errorf("profilePath = %q", got)
	}
	if got := listProfiles(); got != nil {
		t.Errorf("listProfiles with no dir = %v, want nil", got)
	}
	dir := profilesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"b", "a", "bad name!", ".hidden-ok", "toolong" + strings.Repeat("x", 64)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := listProfiles()
	want := []string{".hidden-ok", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("listProfiles = %v, want %v", got, want)
	}
}

func TestIconsAvailable(t *testing.T) {
	sess := newTestSession(t, "font-family = JetBrainsMono Nerd Font\n")

	t.Setenv("SPOOKIUI_ICONS", "1")
	if !iconsAvailable(sess) {
		t.Error("SPOOKIUI_ICONS=1 should force icons on")
	}
	t.Setenv("SPOOKIUI_ICONS", "0")
	if iconsAvailable(sess) {
		t.Error("SPOOKIUI_ICONS=0 should force icons off")
	}
	t.Setenv("SPOOKIUI_ICONS", "")
	if !iconsAvailable(sess) {
		t.Error("Nerd Font family should enable icons")
	}

	sess2 := newTestSession(t, "font-family = Menlo\n")
	if iconsAvailable(sess2) {
		t.Error("non-Nerd font should leave icons off")
	}
}

func TestIconNoticeText(t *testing.T) {
	orig := isMacOS
	defer func() { isMacOS = orig }()
	isMacOS = true
	if txt := iconNoticeText(); !strings.Contains(txt, "font-symbols-only-nerd-font") {
		t.Errorf("macOS notice missing brew hint: %q", txt)
	}
	isMacOS = false
	if txt := iconNoticeText(); !strings.Contains(txt, "nerdfonts.com") {
		t.Errorf("Linux notice missing nerdfonts hint: %q", txt)
	}
}
