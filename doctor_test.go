// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
	"time"
)

func TestRunDoctorHealthy(t *testing.T) {
	sess := newTestSession(t, "")
	findings := runDoctor(sess)
	if len(findings) != 1 || findings[0].Severity != "ok" ||
		findings[0].Message != "no issues found — config looks healthy" {
		t.Errorf("findings = %+v", findings)
	}
}

func TestRunDoctorInvalidConfig(t *testing.T) {
	sess := newTestSession(t, "bogus = 1\n")
	origRun := runCmd
	defer func() { runCmd = origRun }()
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if len(args) >= 2 && args[1] == "+validate-config" {
			return cmdResult{code: 1, stderr: "something is wrong"}, nil
		}
		return cmdResult{}, nil
	}
	findings := runDoctor(sess)
	if findings[0].Severity != "error" || findings[0].Message != "invalid config: something is wrong" {
		t.Errorf("findings[0] = %+v", findings[0])
	}
	// The unknown `bogus` option is also flagged.
	found := false
	for _, f := range findings {
		if f.Severity == "warn" && strings.Contains(f.Message, "unknown option `bogus`") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unknown-option warning in %+v", findings)
	}
}

func TestRunDoctorWarnings(t *testing.T) {
	sess := newTestSession(t,
		"font-size = 12\nfont-size = 14\nwindow-decoration = true\n")
	findings := runDoctor(sess)
	var dup, redundant bool
	for _, f := range findings {
		if f.Severity == "warn" && strings.Contains(f.Message, "`font-size` is set 2×") {
			dup = true
		}
		if f.Severity == "info" && strings.Contains(f.Message, "`window-decoration` is set to its default") {
			redundant = true
		}
	}
	if !dup {
		t.Errorf("expected duplicate warning in %+v", findings)
	}
	if !redundant {
		t.Errorf("expected redundant-default info in %+v", findings)
	}
}

func TestRunDoctorKeybinds(t *testing.T) {
	sess := newTestSession(t,
		"keybind = super+x=quit\nkeybind = super+x=reset\nkeybind = super+c=paste_from_clipboard\n")
	origRun := runCmd
	origCache := defaultKeybindsCache
	defer func() { runCmd, defaultKeybindsCache = origRun, origCache }()
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		if len(args) >= 2 && args[1] == "+list-keybinds" {
			return cmdResult{stdout: "keybind = super+c=copy_to_clipboard\n"}, nil
		}
		return cmdResult{}, nil
	}
	defaultKeybindsCache = nil

	findings := runDoctor(sess)
	var shadow, override bool
	for _, f := range findings {
		if f.Severity == "warn" && strings.Contains(f.Message, "keybind trigger `super+x` is bound 2×") {
			shadow = true
		}
		if f.Severity == "info" && strings.Contains(f.Message,
			"keybind `super+c` overrides Ghostty's default (default is `copy_to_clipboard`)") {
			override = true
		}
	}
	if !shadow {
		t.Errorf("expected shadow warning in %+v", findings)
	}
	if !override {
		t.Errorf("expected override-default info in %+v", findings)
	}
}
