// Copyright (c) 2026 Learned By Error
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setupSSHHOME(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	return home
}

func TestTilde(t *testing.T) {
	home := setupSSHHOME(t, nil)
	if got := tilde(home); got != "~" {
		t.Errorf("tilde(home) = %q", got)
	}
	if got := tilde(filepath.Join(home, ".zshrc")); got != "~/.zshrc" {
		t.Errorf("tilde(rc) = %q", got)
	}
	if got := tilde("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("tilde(other) = %q", got)
	}
}

func TestSSHRCScanFiles(t *testing.T) {
	home := setupSSHHOME(t, map[string]string{
		".bashrc": "", ".zshrc": "", ".other": "",
	})
	got := sshRCScanFiles()
	want := []string{filepath.Join(home, ".zshrc"), filepath.Join(home, ".bashrc")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sshRCScanFiles = %v, want %v", got, want)
	}
}

func TestSSHRCTarget(t *testing.T) {
	home := setupSSHHOME(t, map[string]string{".bash_profile": ""})
	t.Setenv("SHELL", "/bin/bash")
	if got := sshRCTarget(); got != filepath.Join(home, ".bash_profile") {
		t.Errorf("bash with profile: %q", got)
	}
	t.Setenv("SHELL", "/bin/zsh")
	if got := sshRCTarget(); got != filepath.Join(home, ".zshrc") {
		t.Errorf("zsh: %q", got)
	}
	t.Setenv("SHELL", "")
	if got := sshRCTarget(); got != filepath.Join(home, ".zshrc") {
		t.Errorf("no shell: %q", got)
	}

	setupSSHHOME(t, nil)
	t.Setenv("SHELL", "/usr/local/bin/bash")
	if got := sshRCTarget(); got != filepath.Join(homeDir(), ".bashrc") {
		t.Errorf("bash no files: %q", got)
	}
}

func TestFindSSHAlias(t *testing.T) {
	setupSSHHOME(t, map[string]string{
		".zshrc": "# nothing here\nexport EDITOR=vim\n",
	})
	if got := findSSHAlias(); got != "" {
		t.Errorf("no alias: %q", got)
	}

	home := setupSSHHOME(t, map[string]string{
		".zshrc":  "# nothing\n",
		".bashrc": "  alias ssh='TERM=xterm-256color ssh'  \n",
	})
	if got := findSSHAlias(); got != filepath.Join(home, ".bashrc") {
		t.Errorf("alias in .bashrc: %q", got)
	}

	setupSSHHOME(t, map[string]string{
		".zshrc": sshFixMarker + "\n" + sshAliasLine + "\n",
	})
	if got := findSSHAlias(); got == "" {
		t.Error("canonical alias not found")
	}
}

func TestVerifyRC(t *testing.T) {
	home := setupSSHHOME(t, nil)
	t.Setenv("SHELL", "/bin/sh")
	good := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(good, []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, note := verifyRC(good); !ok || note != "rc parses cleanly" {
		t.Errorf("good rc: %v %q", ok, note)
	}
	bad := filepath.Join(home, ".bashrc")
	if err := os.WriteFile(bad, []byte("if true; then\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := verifyRC(bad); ok {
		t.Error("broken rc should fail the -n parse")
	}
}

func TestApplySSHFix(t *testing.T) {
	home := setupSSHHOME(t, nil)
	t.Setenv("SHELL", "/bin/sh")

	ok, msg := applySSHFix()
	if !ok {
		t.Fatalf("apply failed: %s", msg)
	}
	if !strings.Contains(msg, "added the ssh alias to ~/.zshrc") ||
		!strings.Contains(msg, "source ~/.zshrc") {
		t.Errorf("msg = %q", msg)
	}
	data, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	want := "\n" + sshFixMarker + "\n" + sshAliasLine + "\n"
	if string(data) != want {
		t.Errorf("rc contents = %q, want %q", data, want)
	}

	// Idempotent: a second run finds the alias and does nothing.
	ok, msg = applySSHFix()
	if !ok || !strings.Contains(msg, "already fixed") {
		t.Errorf("second run: %v %q", ok, msg)
	}
	data2, _ := os.ReadFile(filepath.Join(home, ".zshrc"))
	if string(data2) != want {
		t.Error("rc modified on idempotent run")
	}
}

func TestSSHFixExplanation(t *testing.T) {
	text := strings.Join(sshFixExplanation, "\n")
	for _, want := range []string{
		"Fix SSH — terminfo over SSH",
		"Error opening terminal: xterm-ghostty",
		sshAliasLine,
		"Safe & idempotent",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("explanation missing %q", want)
		}
	}
}
