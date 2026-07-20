package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGitCheckoutRoot(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitCheckoutRoot(filepath.Join(nested, "spookiui")); got != "" {
		t.Errorf("no .git: got %q, want \"\"", got)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := gitCheckoutRoot(filepath.Join(nested, "spookiui")); got != root {
		t.Errorf("with .git: got %q, want %q", got, root)
	}
}

func TestIsHomebrewInstall(t *testing.T) {
	t.Setenv("HOMEBREW_PREFIX", "")
	if isHomebrewInstall("/usr/local/bin/spookiui") {
		t.Error("plain /usr/local path flagged as Homebrew")
	}
	if !isHomebrewInstall("/opt/homebrew/Cellar/spookiui/2.8.0/bin/spookiui") {
		t.Error("Cellar path not flagged as Homebrew")
	}
	prefix := t.TempDir()
	t.Setenv("HOMEBREW_PREFIX", prefix)
	if !isHomebrewInstall(filepath.Join(prefix, "bin", "spookiui")) {
		t.Error("path under HOMEBREW_PREFIX not flagged as Homebrew")
	}
	if isHomebrewInstall(filepath.Join(t.TempDir(), "spookiui")) {
		t.Error("unrelated path flagged as Homebrew")
	}
}

func TestVerifyBinaryMagic(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"elf", []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0}, true},
		{"macho-64-be", []byte{0xfe, 0xed, 0xfa, 0xcf, 0, 0, 0, 1}, true},
		{"macho-le", []byte{0xcf, 0xfa, 0xed, 0xfe, 7, 0, 0, 1}, true},
		{"macho-fat", []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 2}, true},
		{"macho-fat64", []byte{0xca, 0xfe, 0xba, 0xbf, 0, 0, 0, 2}, true},
		{"text", []byte("#!/bin/sh\necho hi\n"), false},
		{"empty", nil, false},
		{"short", []byte{0x7f, 'E'}, false},
	}
	for _, c := range cases {
		if got := verifyBinaryMagic(c.data); got != c.want {
			t.Errorf("verifyBinaryMagic(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLookupChecksum(t *testing.T) {
	sums := "aaa111  spookiui_darwin_arm64\nbbb222  spookiui_linux_amd64\n"
	if got := lookupChecksum(sums, "spookiui_linux_amd64"); got != "bbb222" {
		t.Errorf("got %q, want bbb222", got)
	}
	if got := lookupChecksum(sums, "spookiui_windows_amd64"); got != "" {
		t.Errorf("missing asset gave %q, want \"\"", got)
	}
	// SHA256-file style prefix (some tools emit "SHA256 (file) = hash") is not
	// supported — we emit/consume the `sha256sum` format only.
}

// stubSelfPath points the self-update machinery at a temp executable.
func stubSelfPath(t *testing.T, path string) {
	t.Helper()
	orig := selfPath
	selfPath = func() string { return path }
	t.Cleanup(func() { selfPath = orig })
}

// stubHTTP replaces httpGet with a URL->bytes map.
func stubHTTP(t *testing.T, pages map[string][]byte) {
	t.Helper()
	orig := httpGet
	httpGet = func(url string, _ time.Duration) ([]byte, error) {
		if b, ok := pages[url]; ok {
			return b, nil
		}
		return nil, fmt.Errorf("no such URL: %s", url)
	}
	t.Cleanup(func() { httpGet = orig })
}

func fakeMachO(tag byte) []byte {
	return append([]byte{0xfe, 0xed, 0xfa, 0xcf}, fill(make([]byte, 64), tag)...)
}

func fill(b []byte, v byte) []byte {
	for i := range b {
		b[i] = v
	}
	return b
}

func TestReplaceSelf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spookiui")
	if err := os.WriteFile(path, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, info := replaceSelf(path, []byte("new-binary"))
	if !ok {
		t.Fatalf("replaceSelf failed: %s", info)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new-binary" {
		t.Fatalf("path contents = %q, err %v", data, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %o, want 755", st.Mode().Perm())
	}
	prev, err := os.ReadFile(path + ".prev")
	if err != nil || string(prev) != "old-binary" {
		t.Errorf(".prev contents = %q, err %v", prev, err)
	}
	if info != path+".prev" {
		t.Errorf("info = %q, want %q", info, path+".prev")
	}
}

func TestSelfUpdateDownloadBranch(t *testing.T) {
	// An "installed" binary outside any git checkout or Homebrew prefix.
	dir := t.TempDir()
	path := filepath.Join(dir, "spookiui")
	if err := os.WriteFile(path, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubSelfPath(t, path)
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "")

	assetName := fmt.Sprintf("spookiui_%s_%s", runtime.GOOS, runtime.GOARCH)
	bin := fakeMachO(0x42)
	sum := fmt.Sprintf("%x", sha256.Sum256(bin))
	stubHTTP(t, map[string][]byte{
		"https://example.invalid/" + assetName:  bin,
		"https://example.invalid/checksums.txt": []byte(sum + "  " + assetName + "\n"),
	})
	stubFetch(t, func(time.Duration) *releaseInfo {
		return &releaseInfo{
			Latest: "v99.0.0",
			URL:    "https://example.invalid/rel",
			Assets: []releaseAsset{
				{Name: assetName, URL: "https://example.invalid/" + assetName},
				{Name: "checksums.txt", URL: "https://example.invalid/checksums.txt"},
				{Name: "spookiui_plan9_mips", URL: "https://example.invalid/other"},
			},
		}
	})

	ok, msg := selfUpdate(nowSeconds())
	if !ok {
		t.Fatalf("selfUpdate failed: %s", msg)
	}
	if !strings.Contains(msg, "updated to v99.0.0") || !strings.Contains(msg, ".prev") {
		t.Errorf("unexpected message: %s", msg)
	}
	data, _ := os.ReadFile(path)
	if string(data[:4]) != string(bin[:4]) {
		t.Error("binary was not replaced with the downloaded asset")
	}
}

func TestSelfUpdateChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "spookiui")
	if err := os.WriteFile(path, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubSelfPath(t, path)
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "")

	assetName := fmt.Sprintf("spookiui_%s_%s", runtime.GOOS, runtime.GOARCH)
	bin := fakeMachO(0x42)
	stubHTTP(t, map[string][]byte{
		"https://example.invalid/" + assetName:  bin,
		"https://example.invalid/checksums.txt": []byte("deadbeef  " + assetName + "\n"),
	})
	stubFetch(t, func(time.Duration) *releaseInfo {
		return &releaseInfo{
			Latest: "v99.0.0",
			Assets: []releaseAsset{
				{Name: assetName, URL: "https://example.invalid/" + assetName},
				{Name: "checksums.txt", URL: "https://example.invalid/checksums.txt"},
			},
		}
	})
	ok, msg := selfUpdate(nowSeconds())
	if ok {
		t.Fatalf("expected failure, got ok with %q", msg)
	}
	if !strings.Contains(msg, "checksum") {
		t.Errorf("message %q does not mention checksum", msg)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old-binary" {
		t.Error("binary was modified despite checksum mismatch")
	}
}

func TestSelfUpdateBranches(t *testing.T) {
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "")

	// Up to date.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubFetch(t, func(time.Duration) *releaseInfo {
		return &releaseInfo{Latest: "v" + version}
	})
	ok, msg := selfUpdate(nowSeconds())
	if !ok || !strings.Contains(msg, "already up to date") {
		t.Errorf("up-to-date branch: ok=%v msg=%q", ok, msg)
	}

	// Unreachable.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubFetch(t, func(time.Duration) *releaseInfo { return nil })
	ok, msg = selfUpdate(nowSeconds())
	if ok || !strings.Contains(msg, "could not reach GitHub") {
		t.Errorf("unreachable branch: ok=%v msg=%q", ok, msg)
	}

	// Homebrew install.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	stubFetch(t, func(time.Duration) *releaseInfo {
		return &releaseInfo{Latest: "v99.0.0"}
	})
	stubSelfPath(t, "/opt/homebrew/Cellar/spookiui/99.0.0/bin/spookiui")
	ok, msg = selfUpdate(nowSeconds())
	if ok || !strings.Contains(msg, "brew upgrade spookiui") {
		t.Errorf("homebrew branch: ok=%v msg=%q", ok, msg)
	}

	// Git checkout: successful pull.
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubSelfPath(t, filepath.Join(repo, "spookiui"))
	origRun := runCmd
	t.Cleanup(func() { runCmd = origRun })
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{stdout: "Already up to date.\n"}, nil
	}
	ok, msg = selfUpdate(nowSeconds())
	if !ok || !strings.Contains(msg, "via git pull") {
		t.Errorf("git branch: ok=%v msg=%q", ok, msg)
	}

	// Git checkout: failed pull reports the last stderr line.
	runCmd = func(args []string, _ time.Duration) (cmdResult, error) {
		return cmdResult{code: 1, stderr: "error: some git problem\nfatal: not possible\n"}, nil
	}
	ok, msg = selfUpdate(nowSeconds())
	if ok || !strings.Contains(msg, "git checkout") || !strings.Contains(msg, "fatal: not possible") {
		t.Errorf("git-fail branch: ok=%v msg=%q", ok, msg)
	}
}
