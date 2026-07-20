package main

import (
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestValidateConfig(t *testing.T) {
	origPath, origRun := ghosttyPath, runCmd
	defer func() { ghosttyPath, runCmd = origPath, origRun }()

	// No ghostty -> always ok.
	ghosttyPath = ""
	if ok, errs := validateConfig("anything"); !ok || errs != nil {
		t.Errorf("no ghostty: %v %v", ok, errs)
	}

	ghosttyPath = "/stub/ghostty"
	// Clean run -> ok.
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{}, nil
	}
	if ok, errs := validateConfig("font-size = 13\n"); !ok || errs != nil {
		t.Errorf("clean: %v %v", ok, errs)
	}

	// Errors: the temp path is stripped from each line.
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		tmp := strings.TrimPrefix(args[len(args)-1], "--config-file=")
		return cmdResult{code: 1, stderr: tmp + ":3: unknown option: bogus\n" + tmp + ": warning line\n"}, nil
	}
	ok, errs := validateConfig("bogus = 1\n")
	if ok {
		t.Error("expected invalid")
	}
	want := []string{"3: unknown option: bogus", "warning line"}
	if !reflect.DeepEqual(errs, want) {
		t.Errorf("errs = %v, want %v", errs, want)
	}
}

func TestReloadMacOS(t *testing.T) {
	origRun := runCmd
	defer func() { runCmd = origRun }()

	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if args[0] != "osascript" {
			t.Errorf("unexpected command: %v", args)
		}
		return cmdResult{}, nil
	}
	if ok, msg := reloadMacOS(); !ok || msg != "reloaded" {
		t.Errorf("success: %v %q", ok, msg)
	}

	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{code: 1, stderr: "osascript is not allowed assistive access. (1002)"}, nil
	}
	if ok, msg := reloadMacOS(); ok || !strings.Contains(msg, "Accessibility permission") {
		t.Errorf("assistive: %v %q", ok, msg)
	}

	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{code: 1, stderr: "Can't get process \"Ghostty\""}, nil
	}
	if ok, msg := reloadMacOS(); ok || msg != "Ghostty doesn't appear to be running" {
		t.Errorf("not running: %v %q", ok, msg)
	}
}

func TestGhosttyPIDs(t *testing.T) {
	origRun := runCmd
	defer func() { runCmd = origRun }()

	self := os.Getpid()
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if reflect.DeepEqual(args, []string{"pgrep", "-x", "ghostty"}) {
			return cmdResult{stdout: "123\n" + strconv.Itoa(self) + "\n456\n123\n"}, nil
		}
		return cmdResult{}, nil
	}
	got := ghosttyPIDs()
	if !reflect.DeepEqual(got, []int{123, 456}) {
		t.Errorf("ghosttyPIDs = %v, want [123 456] (self excluded, deduped)", got)
	}

	// Fallback to the second pgrep form when the first finds nothing.
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if reflect.DeepEqual(args, []string{"pgrep", "-x", "ghostty"}) {
			return cmdResult{stdout: ""}, nil
		}
		if reflect.DeepEqual(args, []string{"pgrep", "-if", "ghostty"}) {
			return cmdResult{stdout: "42\n"}, nil
		}
		return cmdResult{}, nil
	}
	if got := ghosttyPIDs(); !reflect.DeepEqual(got, []int{42}) {
		t.Errorf("fallback ghosttyPIDs = %v, want [42]", got)
	}
}

func TestReloadLinux(t *testing.T) {
	origPIDs := ghosttyPIDs
	defer func() { ghosttyPIDs = origPIDs }()

	ghosttyPIDs = func() []int { return nil }
	if ok, msg := reloadLinux(); ok || msg != "Ghostty doesn't appear to be running" {
		t.Errorf("no pids: %v %q", ok, msg)
	}

	// A pid that cannot exist -> ESRCH -> still "not running".
	ghosttyPIDs = func() []int { return []int{1 << 22} }
	if ok, msg := reloadLinux(); ok || msg != "Ghostty doesn't appear to be running" {
		t.Errorf("ESRCH: %v %q", ok, msg)
	}

	// EPERM (pid 1, when not root).
	if os.Geteuid() != 0 {
		ghosttyPIDs = func() []int { return []int{1} }
		if ok, msg := reloadLinux(); ok || !strings.Contains(msg, "not permitted to signal Ghostty (pid 1)") {
			t.Errorf("EPERM: %v %q", ok, msg)
		}
	}

	// Success: signal one of our own children (SIGUSR2's default action
	// terminates it, which is fine — we reap it below).
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	ghosttyPIDs = func() []int { return []int{cmd.Process.Pid} }
	if ok, msg := reloadLinux(); !ok || msg != "reloaded" {
		t.Errorf("success: %v %q", ok, msg)
	}
}

func TestIsGhosttyRunning(t *testing.T) {
	origRun := runCmd
	defer func() { runCmd = origRun }()

	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if reflect.DeepEqual(args, []string{"pgrep", "-x", "ghostty"}) {
			return cmdResult{stdout: "123\n"}, nil
		}
		return cmdResult{}, nil
	}
	if !isGhosttyRunning() {
		t.Error("should detect running ghostty")
	}
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stdout: ""}, nil
	}
	if isGhosttyRunning() {
		t.Error("should detect not running")
	}
}

func TestListThemes(t *testing.T) {
	origPath, origRun := ghosttyPath, runCmd
	defer func() { ghosttyPath, runCmd = origPath, origRun }()

	ghosttyPath = ""
	if got := listThemes(); got != nil {
		t.Errorf("no ghostty: %v", got)
	}

	ghosttyPath = "/stub/ghostty"
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stdout: "Dracula (resources)\nMy Theme (user)\nPlain\n\n"}, nil
	}
	want := []string{"Dracula", "My Theme", "Plain"}
	if got := listThemes(); !reflect.DeepEqual(got, want) {
		t.Errorf("listThemes = %v, want %v", got, want)
	}

	// Falls back to stderr when stdout is empty.
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stderr: "ErrTheme (user)\n"}, nil
	}
	if got := listThemes(); !reflect.DeepEqual(got, []string{"ErrTheme"}) {
		t.Errorf("stderr fallback = %v", got)
	}
}

func TestListFonts(t *testing.T) {
	origPath, origRun := ghosttyPath, runCmd
	defer func() { ghosttyPath, runCmd = origPath, origRun }()
	ghosttyPath = "/stub/ghostty"
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stdout: "Menlo\n  Menlo Bold\nMonaco\nMenlo\n\n"}, nil
	}
	want := []string{"Menlo", "Monaco"}
	if got := listFonts(); !reflect.DeepEqual(got, want) {
		t.Errorf("listFonts = %v, want %v", got, want)
	}
}

func TestListActions(t *testing.T) {
	origPath, origRun := ghosttyPath, runCmd
	origCache := actionsCache
	defer func() { ghosttyPath, runCmd, actionsCache = origPath, origRun, origCache }()
	ghosttyPath = "/stub/ghostty"
	calls := 0
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		calls++
		return cmdResult{stdout: "copy_to_clipboard\nNot an action\nincrease_font_size\ncopy_to_clipboard\n"}, nil
	}
	actionsCache = nil
	want := []string{"copy_to_clipboard", "increase_font_size"}
	if got := listActions(); !reflect.DeepEqual(got, want) {
		t.Errorf("listActions = %v, want %v", got, want)
	}
	listActions()
	if calls != 1 {
		t.Errorf("listActions should cache, calls = %d", calls)
	}
}

func TestListDefaultKeybinds(t *testing.T) {
	origPath, origRun := ghosttyPath, runCmd
	origCache := defaultKeybindsCache
	defer func() { ghosttyPath, runCmd, defaultKeybindsCache = origPath, origRun, origCache }()
	ghosttyPath = "/stub/ghostty"
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stdout: "keybind = super+c=copy_to_clipboard\nsome other line\nkeybind = super+==increase_font_size\n"}, nil
	}
	defaultKeybindsCache = nil
	want := map[string]string{
		"super+c": "copy_to_clipboard",
		"super+=": "increase_font_size",
	}
	if got := listDefaultKeybinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("listDefaultKeybinds = %v, want %v", got, want)
	}
}
