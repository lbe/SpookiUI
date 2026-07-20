// SpookiUI — a live configurator for the Ghostty terminal (Go port).
//
// Run with no arguments to launch the interactive TUI (see runTUI). A
// scriptable CLI mirrors the Python original:
// get, set, list, doc, reset, version, update, profile, doctor, fix-ssh,
// reload, validate, themes, fonts, path. Run `spookiui --help`.
//
// On startup SpookiUI checks GitHub for a newer release (cached for a day;
// set SPOOKIUI_NO_UPDATE_CHECK=1 to disable) and shows a badge if one is
// available (TUI path only).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// ---- Version & repository ----

const version = "2.8.0"
const githubRepo = "lbe/SpookiUI"

// ---- Process execution (_run) ----

// cmdResult mirrors the pieces of subprocess.CompletedProcess we use.
type cmdResult struct {
	stdout string
	stderr string
	code   int
}

// runCmd is the package-level command runner (Python's `_run`). It is a
// variable so tests can substitute a fake.
var runCmd = func(args []string, timeout time.Duration) (cmdResult, error) {
	var res cmdResult
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	res.stdout = outBuf.String()
	res.stderr = errBuf.String()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			res.code = exitErr.ExitCode()
			return res, nil
		}
		return res, err // not found, timeout, failed to start
	}
	return res, nil
}

// ---- Ghostty discovery & platform ----

// findGhostty returns a path to the `ghostty` executable, or "" if not found.
func findGhostty() string {
	if exe, err := exec.LookPath("ghostty"); err == nil {
		return exe
	}
	for _, cand := range []string{
		"/Applications/Ghostty.app/Contents/MacOS/ghostty",
		filepath.Join(homeDir(), "Applications/Ghostty.app/Contents/MacOS/ghostty"),
		"/usr/bin/ghostty",
		"/usr/local/bin/ghostty",
	} {
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

// ghosttyPath is Python's module-level GHOSTTY. A variable so tests can stub.
var ghosttyPath = findGhostty()

var (
	isMacOS         = runtime.GOOS == "darwin"
	isLinux         = runtime.GOOS == "linux"
	canReload       = isMacOS || isLinux
	currentPlatform = func() string {
		if isMacOS {
			return "macos"
		}
		if isLinux {
			return "linux"
		}
		return ""
	}()
)

// homeDir is Python's os.path.expanduser("~").
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// tilde renders an absolute path under $HOME as ~/… for friendlier messages.
func tilde(path string) string {
	home := homeDir()
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// ---- Update check (24h JSON cache in the XDG cache dir) ----

const updateCheckTTL = 24 * 60 * 60 // seconds

var releasesAPI = "https://api.github.com/repos/" + githubRepo + "/releases/latest"
var releasesURL = "https://github.com/" + githubRepo + "/releases/latest"

func updateCheckDisabled() bool {
	v := strings.TrimSpace(os.Getenv("SPOOKIUI_NO_UPDATE_CHECK"))
	return v != "" && v != "0"
}

func updateCacheFile() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".cache")
	}
	return filepath.Join(base, "spookiui", "update-check.json")
}

var digitRunRe = regexp.MustCompile(`\d+`)

// parseVersion turns "v1.10.2", "1.2.0-beta" etc. into a comparable numeric
// slice. Anything without digits parses to [0].
func parseVersion(v string) []int {
	v = strings.TrimSpace(v)
	v = strings.TrimLeft(v, "vV")
	core := strings.Split(strings.Split(v, "-")[0], "+")[0]
	nums := digitRunRe.FindAllString(core, -1)
	if len(nums) == 0 {
		return []int{0}
	}
	out := make([]int, len(nums))
	for i, n := range nums {
		v, err := strconv.Atoi(n)
		if err != nil {
			continue // unreachable: n is a run of digits
		}
		out[i] = v
	}
	return out
}

// compareVersions mirrors Python tuple comparison (prefix-equal shorter is smaller).
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func isNewer(latest, current string) bool {
	return compareVersions(parseVersion(latest), parseVersion(current)) > 0
}

// updateCache is the on-disk JSON cache, same shape as the Python's.
type updateCache struct {
	CheckedAt float64 `json:"checked_at"`
	Latest    string  `json:"latest"`
	URL       string  `json:"url"`
	Notes     string  `json:"notes"`
}

func readUpdateCache() *updateCache {
	data, err := os.ReadFile(updateCacheFile())
	if err != nil {
		return nil
	}
	var c updateCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func writeUpdateCache(c *updateCache) {
	path := updateCacheFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	// Best-effort cache write; failures are harmless (Python: `except OSError: pass`).
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}
}

// releaseAsset is one downloadable file attached to a GitHub release.
type releaseAsset struct {
	Name string
	URL  string
}

// releaseInfo is what the GitHub latest-release API gives us.
type releaseInfo struct {
	Latest string
	URL    string
	Notes  string
	Assets []releaseAsset
}

// httpGet fetches a URL. A variable so tests never touch the network.
var httpGet = func(url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "SpookiUI/"+version)
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// fetchLatestRelease hits the GitHub API for the latest release.
// Returns nil on any problem. A variable so tests substitute a fake.
var fetchLatestRelease = func(timeout time.Duration) *releaseInfo {
	body, err := httpGet(releasesAPI, timeout)
	if err != nil {
		return nil
	}
	var payload struct {
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	tag := payload.TagName
	if tag == "" {
		tag = payload.Name
	}
	if tag == "" {
		return nil
	}
	url := payload.HTMLURL
	if url == "" {
		url = releasesURL
	}
	ri := &releaseInfo{Latest: tag, URL: url, Notes: strings.TrimSpace(payload.Body)}
	for _, a := range payload.Assets {
		ri.Assets = append(ri.Assets, releaseAsset{Name: a.Name, URL: a.BrowserDownloadURL})
	}
	return ri
}

// updateInfo is checkForUpdate's result.
type updateInfo struct {
	Latest   string
	URL      string
	Notes    string
	Current  string
	Outdated bool
}

// checkForUpdate returns update info, cached to at most one network call per
// day. Returns nil only when we have no information at all. `force` bypasses
// both the opt-out and the cache TTL. `now` is injectable like the Python's.
func checkForUpdate(force bool, now float64) *updateInfo {
	if updateCheckDisabled() && !force {
		return nil
	}
	cache := readUpdateCache()
	fresh := cache != nil && now-cache.CheckedAt < updateCheckTTL
	var latest string
	if cache != nil && fresh && !force {
		latest = cache.Latest
	} else {
		got := fetchLatestRelease(4 * time.Second)
		if got == nil {
			if cache == nil {
				return nil
			}
			latest = cache.Latest
		} else {
			latest = got.Latest
			cache = &updateCache{CheckedAt: now, Latest: got.Latest, URL: got.URL, Notes: got.Notes}
			writeUpdateCache(cache)
		}
	}
	if latest == "" {
		return nil
	}
	url := releasesURL
	notes := ""
	if cache != nil {
		if cache.URL != "" {
			url = cache.URL
		}
		notes = cache.Notes
	}
	return &updateInfo{
		Latest:   latest,
		URL:      url,
		Notes:    notes,
		Current:  version,
		Outdated: isNewer(latest, version),
	}
}

// nowSeconds is time.time() for checkForUpdate/selfUpdate callers.
func nowSeconds() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ---- Self update ----

// selfPath is the absolute, symlink-resolved path of the running executable
// (replaces Python's self_path() which pointed at the .py file). A variable
// so tests can point it at a temp binary.
var selfPath = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		p = rp
	}
	if ap, err := filepath.Abs(p); err == nil {
		p = ap
	}
	return p
}

// gitCheckoutRoot: if path lives inside a git working tree, return the root.
func gitCheckoutRoot(path string) string {
	d := filepath.Dir(path)
	for {
		if fileExists(filepath.Join(d, ".git")) {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// realpath resolves symlinks like Python's os.path.realpath: it works even
// when the final component does not exist, by resolving the deepest existing
// ancestor and re-appending the remainder.
func realpath(path string) string {
	if rp, err := filepath.EvalSymlinks(path); err == nil {
		return rp
	}
	dir := filepath.Dir(path)
	if dir == path {
		return path
	}
	return filepath.Join(realpath(dir), filepath.Base(path))
}

// isHomebrewInstall: whether path is a Homebrew-managed copy (in a Cellar or
// under the brew prefix). Such installs must be updated with `brew upgrade`.
func isHomebrewInstall(path string) bool {
	rp := realpath(path)
	if strings.Contains(rp, string(os.PathSeparator)+"Cellar"+string(os.PathSeparator)) {
		return true
	}
	prefix := os.Getenv("HOMEBREW_PREFIX")
	if prefix != "" {
		if strings.HasPrefix(rp, realpath(prefix)+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// verifyBinaryMagic checks the downloaded asset really is a Mach-O or ELF
// executable before we trust it (replaces Python's py_compile source check).
func verifyBinaryMagic(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	if bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return true
	}
	magics := map[uint32]bool{
		0xfeedface: true, 0xfeedfacf: true, // Mach-O 32/64
		0xcafebabe: true, 0xcafebabf: true, // Mach-O fat / fat64
	}
	be := uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
	le := uint32(data[3])<<24 | uint32(data[2])<<16 | uint32(data[1])<<8 | uint32(data[0])
	return magics[be] || magics[le]
}

// lookupChecksum finds the sha256 for assetName in a sha256sum-format file.
func lookupChecksum(sumsText, assetName string) string {
	for _, line := range strings.Split(sumsText, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == assetName {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

// replaceSelf atomically replaces path, keeping a .prev backup.
// Returns (ok, info) where info is the backup path on success.
func replaceSelf(path string, data []byte) (bool, string) {
	dir := filepath.Dir(path)
	st, err := os.Stat(path)
	if err != nil {
		return false, fmt.Sprintf("failed to stat %s: %v", path, err)
	}
	// Best-effort backup of the current binary (like Python's copy2).
	if old, readErr := os.ReadFile(path); readErr == nil {
		//nolint:errcheck // best-effort: backup failure is non-fatal, the update still proceeds
		_ = os.WriteFile(path+".prev", old, st.Mode())
	}
	tmp, err := os.CreateTemp(dir, ".spookiui-*.tmp")
	if err != nil {
		return false, fmt.Sprintf("no write permission for %s — re-run with sudo or reinstall", path)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Sprintf("failed to write update: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Sprintf("failed to write update: %v", err)
	}
	if err := os.Chmod(tmpName, st.Mode()); err != nil {
		return false, fmt.Sprintf("failed to write update: %v", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Sprintf("failed to write update: %v", err)
	}
	return true, path + ".prev"
}

// selfUpdate updates this binary in place to the latest release.
// Returns (ok, message).
func selfUpdate(now float64) (bool, string) {
	info := checkForUpdate(true, now)
	if info == nil {
		return false, "could not reach GitHub to check for updates"
	}
	if !info.Outdated {
		return true, fmt.Sprintf("already up to date (v%s)", version)
	}
	tag := info.Latest
	path := selfPath()

	if isHomebrewInstall(path) {
		return false, fmt.Sprintf("installed via Homebrew — run `brew upgrade spookiui` to get %s", tag)
	}

	if repo := gitCheckoutRoot(path); repo != "" {
		proc, err := runCmd([]string{"git", "-C", repo, "pull", "--ff-only"}, 60*time.Second)
		if err == nil && proc.code == 0 {
			return true, fmt.Sprintf("updated to %s via git pull — restart SpookiUI to run it", tag)
		}
		out := proc.stderr
		if out == "" {
			out = proc.stdout
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		hint := "git pull failed"
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			hint = strings.TrimSpace(lines[len(lines)-1])
		}
		return false, fmt.Sprintf("this is a git checkout; run `git pull` yourself in %s\n  (%s)", repo, hint)
	}

	// Standalone binary: download the matching release asset, verify it, and
	// atomically swap it in (the Python downloaded its own source instead).
	rel := fetchLatestRelease(20 * time.Second)
	if rel == nil {
		return false, fmt.Sprintf("failed to download %s from GitHub", tag)
	}
	assetName := fmt.Sprintf("spookiui_%s_%s", runtime.GOOS, runtime.GOARCH)
	var assetURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case assetName:
			assetURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if assetURL == "" {
		return false, fmt.Sprintf("release %s has no asset named %s", tag, assetName)
	}
	if sumsURL == "" {
		return false, fmt.Sprintf("release %s has no checksums.txt", tag)
	}
	binData, err := httpGet(assetURL, 120*time.Second)
	if err != nil {
		return false, fmt.Sprintf("failed to download %s from GitHub", tag)
	}
	sumsData, err := httpGet(sumsURL, 20*time.Second)
	if err != nil {
		return false, fmt.Sprintf("failed to download checksums for %s", tag)
	}
	want := lookupChecksum(string(sumsData), assetName)
	if want == "" {
		return false, fmt.Sprintf("checksums.txt has no entry for %s", assetName)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(binData)); got != want {
		return false, "checksum mismatch on downloaded binary — update aborted"
	}
	if !verifyBinaryMagic(binData) {
		return false, "downloaded file is not a Mach-O/ELF binary — update aborted"
	}
	ok, res := replaceSelf(path, binData)
	if !ok {
		return false, res
	}
	return true, fmt.Sprintf("updated to %s — restart SpookiUI to run it (previous version saved as %s)",
		tag, filepath.Base(res))
}

// ---- Option tables & schema ----

var forceList = map[string]bool{
	"font-family": true, "font-family-bold": true, "font-family-italic": true,
	"font-family-bold-italic": true, "font-feature": true, "font-variation": true,
	"font-variation-bold": true, "font-variation-italic": true,
	"font-variation-bold-italic": true, "font-codepoint-map": true,
	"keybind": true, "palette": true, "config-file": true,
	"config-default-files": true, "env": true, "clipboard-codepoint-map": true,
	"key-remap": true, "command-palette-entry": true, "link": true,
}

var colorHints = []string{
	"background", "foreground", "cursor-color", "cursor-text", "bold-color",
	"split-divider-color", "unfocused-split-fill", "window-padding-color",
	"window-titlebar-background", "window-titlebar-foreground",
}

// sliderRanges: name -> (min, max, step).
var sliderRanges = map[string][3]float64{
	"minimum-contrast":         {1.0, 21.0, 0.5},
	"bell-audio-volume":        {0.0, 1.0, 0.05},
	"background-image-opacity": {0.0, 1.0, 0.05},
}

// Option is one Ghostty config option, parsed from `+show-config --docs`.
// Platform is "" for cross-platform (Python's None).
type Option struct {
	Name       string
	Default    string
	Defaults   []string
	Doc        string
	Values     []string
	Kind       string // text, bool, enum, int, float, color, theme, palette, keybind, font, list
	IsList     bool
	ReloadNote string
	Category   string
	Platform   string // "", "macos" or "linux"
}

func (o *Option) isColor() bool { return o.Kind == "color" }

// sliderRange returns (min, max, step, true) if this option should use a
// slider. Kept tolerant of Ghostty version drift: any future 0-1 opacity
// option is picked up from its docs without needing to be listed explicitly.
func sliderRange(opt *Option) (min, max, step float64, ok bool) {
	if r, found := sliderRanges[opt.Name]; found {
		return r[0], r[1], r[2], true
	}
	if opt.Kind == "float" {
		d := strings.ToLower(opt.Doc)
		if strings.Contains(d, "fully opaque") && strings.Contains(d, "fully transparent") {
			return 0.0, 1.0, 0.05, true
		}
	}
	return 0, 0, 0, false
}

var (
	intRe      = regexp.MustCompile(`^-?\d+$`)
	floatRe    = regexp.MustCompile(`^-?\d*\.\d+$`)
	backtickRe = regexp.MustCompile("`([^`]+)`")
	enumDashRe = regexp.MustCompile(`\s[-—]\s`)
)

// enumValuesFromDoc pulls enum choices from a bulleted doc block. Ghostty
// sometimes packs several values onto one bullet and wraps the list onto
// continuation lines, so we gather backtick tokens from each bullet *and* its
// continuation lines — but only the part before the ` - `/` — ` description,
// so backticked terms in prose aren't mistaken for values.
func enumValuesFromDoc(doc string) []string {
	var vals []string
	inItem := false
	for _, raw := range strings.Split(doc, "\n") {
		stripped := strings.TrimSpace(raw)
		var content string
		switch {
		case strings.HasPrefix(stripped, "*"):
			inItem = true
			content = stripped[1:]
		case inItem && strings.HasPrefix(stripped, "`"):
			content = stripped
		default:
			inItem = false
			continue
		}
		left := enumDashRe.Split(content, 2)[0]
		for _, m := range backtickRe.FindAllStringSubmatch(left, -1) {
			vals = append(vals, m[1])
		}
	}
	return vals
}

func classify(opt *Option) {
	name, dflt := opt.Name, opt.Default
	docLow := strings.ToLower(opt.Doc)

	switch {
	case strings.Contains(docLow, "cannot be reloaded at runtime") ||
		strings.Contains(docLow, "fully restart") ||
		strings.Contains(docLow, "must fully restart"):
		opt.ReloadNote = "needs full Ghostty restart"
	case strings.Contains(docLow, "only applies to new windows") ||
		strings.Contains(docLow, "only affects new windows"):
		opt.ReloadNote = "applies to new windows only"
	case strings.Contains(docLow, "will not affect existing"):
		opt.ReloadNote = "affects new surfaces only"
	}

	switch {
	case name == "theme":
		opt.Kind = "theme"
		return
	case name == "palette":
		opt.Kind, opt.IsList = "palette", true
		return
	case name == "keybind":
		opt.Kind, opt.IsList = "keybind", true
		return
	case strings.HasPrefix(name, "font-family"):
		opt.Kind, opt.IsList = "font", true
		return
	}

	if dflt == "true" || dflt == "false" {
		opt.Kind = "bool"
		opt.Values = []string{"true", "false"}
		return
	}

	var cands []string
	for _, c := range enumValuesFromDoc(opt.Doc) {
		if !strings.Contains(c, " ") && len(c) <= 32 {
			cands = append(cands, c)
		}
	}
	if len(cands) > 0 && dflt != "" && containsString(cands, dflt) {
		opt.Kind = "enum"
		seen := map[string]bool{}
		var uniq []string
		for _, c := range cands {
			if !seen[c] {
				seen[c] = true
				uniq = append(uniq, c)
			}
		}
		opt.Values = uniq
		return
	}

	if opt.IsList {
		opt.Kind = "list"
		return
	}

	if intRe.MatchString(dflt) {
		if strings.Contains(name, "opacity") || name == "minimum-contrast" ||
			name == "mouse-scroll-multiplier" || name == "bell-audio-volume" {
			opt.Kind = "float"
		} else {
			opt.Kind = "int"
		}
		return
	}
	if floatRe.MatchString(dflt) {
		opt.Kind = "float"
		return
	}

	colorish := strings.HasSuffix(name, "-color") ||
		strings.HasPrefix(name, "selection-") || strings.HasPrefix(name, "search-")
	if !colorish {
		for _, h := range colorHints {
			if strings.Contains(name, h) {
				colorish = true
				break
			}
		}
	}
	if colorish && len(opt.Values) == 0 {
		opt.Kind = "color"
		return
	}

	opt.Kind = "text"
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var categoryOrder = []string{
	"Colors & Theme", "Font", "Cursor", "Window", "Spacing & Metrics",
	"Mouse", "Clipboard & Selection", "Quick Terminal", "Shell & Commands",
	"Keybindings", "macOS", "Linux / GTK", "Advanced",
}

// utilsCategory is a synthetic left-pane category that isn't schema-backed:
// it lists one-shot maintenance actions (e.g. Fix SSH). (TUI phase.)
const utilsCategory = "⚙ Utils"

// Nerd Font glyphs shown beside each root category when a Nerd Font is in
// use (see iconsAvailable). Codepoints are FontAwesome-range nf-fa-* icons.
var categoryIcons = map[string]string{
	"Colors & Theme":        "\uf1fc", // paint brush
	"Font":                  "\uf031", // font
	"Cursor":                "\uf246", // i-cursor
	"Window":                "\uf2d0", // window
	"Spacing & Metrics":     "\uf0b2", // arrows
	"Mouse":                 "\uf245", // mouse pointer
	"Clipboard & Selection": "\uf0ea", // clipboard
	"Quick Terminal":        "\uf120", // terminal
	"Shell & Commands":      "\uf121", // code
	"Keybindings":           "\uf11c", // keyboard
	"macOS":                 "\uf179", // apple
	"Linux / GTK":           "\uf17c", // linux
	"Advanced":              "\uf085", // cogs
	utilsCategory:           "\uf013", // cog
}

const defaultCategoryIcon = "\uf07b" // folder

func categorize(name string) string {
	n := name
	switch n {
	case "theme", "palette", "foreground", "background", "background-opacity",
		"background-blur", "background-image", "minimum-contrast",
		"bold-color", "faint-opacity", "alpha-blending", "split-divider-color",
		"unfocused-split-fill", "unfocused-split-opacity", "window-colorspace",
		"background-opacity-cells", "background-image-fit",
		"background-image-opacity", "background-image-position",
		"background-image-repeat", "osc-color-report-format",
		"palette-generate", "palette-harmonious":
		return "Colors & Theme"
	}
	if strings.HasSuffix(n, "-color") || strings.HasPrefix(n, "selection-") ||
		strings.HasPrefix(n, "search-") {
		return "Colors & Theme"
	}
	if strings.HasPrefix(n, "cursor") {
		return "Cursor"
	}
	if strings.HasPrefix(n, "font") {
		return "Font"
	}
	if strings.HasPrefix(n, "adjust-") || strings.HasPrefix(n, "window-padding") ||
		n == "grapheme-width-method" || n == "freetype-load-flags" {
		return "Spacing & Metrics"
	}
	if strings.HasPrefix(n, "window") {
		return "Window"
	}
	switch n {
	case "maximize", "fullscreen", "initial-window",
		"resize-overlay", "resize-overlay-duration",
		"resize-overlay-position", "class",
		"title", "title-report", "undo-timeout",
		"auto-update", "auto-update-channel",
		"quit-after-last-window-closed",
		"quit-after-last-window-closed-delay",
		"confirm-close-surface":
		return "Window"
	}
	if strings.HasPrefix(n, "mouse") {
		return "Mouse"
	}
	switch n {
	case "focus-follows-mouse", "click-repeat-interval",
		"cursor-click-to-move", "right-click-action",
		"link", "link-url", "link-previews":
		return "Mouse"
	}
	if strings.HasPrefix(n, "clipboard") {
		return "Clipboard & Selection"
	}
	switch n {
	case "copy-on-select", "selection-word-chars",
		"selection-clear-on-copy", "selection-clear-on-typing":
		return "Clipboard & Selection"
	}
	if strings.HasPrefix(n, "quick-terminal") {
		return "Quick Terminal"
	}
	switch n {
	case "shell-integration", "shell-integration-features", "command",
		"initial-command", "working-directory", "env", "term",
		"wait-after-command", "enquiry-response",
		"abnormal-command-exit-runtime", "notify-on-command-finish",
		"notify-on-command-finish-action", "notify-on-command-finish-after",
		"scrollback-limit", "scroll-to-bottom", "image-storage-limit":
		return "Shell & Commands"
	}
	if strings.HasPrefix(n, "keybind") {
		return "Keybindings"
	}
	switch n {
	case "input", "key-remap", "macos-shortcuts",
		"command-palette-entry", "vt-kam-allowed":
		return "Keybindings"
	}
	if strings.HasPrefix(n, "macos") {
		return "macOS"
	}
	if strings.HasPrefix(n, "gtk") || strings.HasPrefix(n, "x11") || strings.HasPrefix(n, "linux") {
		return "Linux / GTK"
	}
	return "Advanced"
}

var macOnlyHints = []string{
	"only supported on macos", "only implemented on macos", "only works on macos",
	"supported currently on macos", "no effect on linux", "no effect on other",
	"only visible with the native macos",
}

var linuxOnlyHints = []string{
	"only supported on linux", "only implemented on linux", "only supported on gtk",
	"only supported in the gtk", "only applies to gtk", "only affects gtk builds",
	"relevant on linux", "only has an effect on linux",
	"feature is only supported on gtk", "configuration only applies to gtk",
	"no effect on macos",
}

var crossPlatformHints = []string{
	"macos and linux", "macos and certain linux", "macos and on some linux",
	"macos and some linux", "linux and macos", "on macos and", "macos, linux",
	"macos and windows",
}

// platformOf returns "macos" or "linux" if the option is exclusive to that
// OS, else "". Keys off the option name prefix first (unambiguous) and falls
// back to scanning the scraped docs.
func platformOf(opt *Option) string {
	name := opt.Name
	if strings.HasPrefix(name, "macos") {
		return "macos"
	}
	if strings.HasPrefix(name, "gtk") || strings.HasPrefix(name, "x11") ||
		strings.HasPrefix(name, "adw") || strings.HasPrefix(name, "linux") {
		return "linux"
	}
	doc := strings.ToLower(opt.Doc)
	for _, p := range crossPlatformHints {
		if strings.Contains(doc, p) {
			return ""
		}
	}
	macOnly, linuxOnly := false, false
	for _, p := range macOnlyHints {
		if strings.Contains(doc, p) {
			macOnly = true
			break
		}
	}
	for _, p := range linuxOnlyHints {
		if strings.Contains(doc, p) {
			linuxOnly = true
			break
		}
	}
	if macOnly && !linuxOnly {
		return "macos"
	}
	if linuxOnly && !macOnly {
		return "linux"
	}
	return ""
}

// platformVisible: whether an option should be shown on the current OS.
func platformVisible(opt *Option) bool {
	if currentPlatform == "" || opt.Platform == "" {
		return true
	}
	return opt.Platform == currentPlatform
}

var schemaKeyRe = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*)\s*=\s?(.*)$`)

// parseSchema parses `ghostty +show-config --default --docs` output into
// typed Options.
func parseSchema(out string) map[string]*Option {
	options := map[string]*Option{}
	var docBuf []string
	for _, raw := range strings.Split(out, "\n") {
		if strings.HasPrefix(raw, "#") {
			content := raw[1:]
			content = strings.TrimPrefix(content, " ")
			docBuf = append(docBuf, content)
			continue
		}
		if strings.TrimSpace(raw) == "" {
			docBuf = nil
			continue
		}
		m := schemaKeyRe.FindStringSubmatch(raw)
		if m == nil {
			docBuf = nil
			continue
		}
		name, value := m[1], m[2]
		if o, exists := options[name]; exists {
			o.IsList = true
			o.Defaults = append(o.Defaults, value)
		} else {
			o := &Option{
				Name: name, Default: value,
				Doc:  strings.TrimSpace(strings.Join(docBuf, "\n")),
				Kind: "text", Category: "Advanced",
			}
			if value != "" {
				o.Defaults = []string{value}
			}
			options[name] = o
		}
		docBuf = nil
	}
	for name, o := range options {
		if forceList[name] {
			o.IsList = true
		}
		classify(o)
		o.Category = categorize(name)
		o.Platform = platformOf(o)
	}
	return options
}

// loadSchema runs the ghostty binary and parses its config documentation.
func loadSchema() (map[string]*Option, error) {
	if ghosttyPath == "" {
		return nil, fmt.Errorf("ghostty binary not found on PATH")
	}
	proc, err := runCmd([]string{ghosttyPath, "+show-config", "--default", "--docs"}, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to read ghostty defaults: %w", err)
	}
	if proc.code != 0 && proc.stdout == "" {
		return nil, fmt.Errorf("failed to read ghostty defaults: %s", strings.TrimSpace(proc.stderr))
	}
	return parseSchema(proc.stdout), nil
}

// ---- Config file location ----

// configPath is the best-effort path to the active Ghostty config file.
func configPath() string {
	var candidates []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "ghostty", "config"))
	}
	candidates = append(candidates, filepath.Join(homeDir(), ".config", "ghostty", "config"))
	if isMacOS {
		candidates = append(candidates, filepath.Join(homeDir(),
			"Library", "Application Support", "com.mitchellh.ghostty", "config"))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return candidates[0]
}

// ---- ConfigFile: a Ghostty config file edited while preserving layout ----

var configKeyRe = regexp.MustCompile(`^(\s*)([a-z0-9][a-z0-9-]*)(\s*=\s*)(.*?)(\s*)$`)

const managedHeader = "# ─────────── added by SpookiUI ───────────"

var legacyHeaders = []string{"# ─────────── added by GhostlyConfig ───────────"}

type ConfigFile struct {
	Path  string
	Lines []string
}

func NewConfigFile(path string) *ConfigFile {
	cf := &ConfigFile{Path: path}
	cf.Reload()
	return cf
}

func (cf *ConfigFile) Reload() {
	data, err := os.ReadFile(cf.Path)
	if err != nil {
		cf.Lines = nil
		return
	}
	cf.Lines = strings.Split(string(data), "\n")
}

// keyAt returns (name, rawValue, true) if line i is a config entry.
func (cf *ConfigFile) keyAt(i int) (string, string, bool) {
	line := cf.Lines[i]
	stripped := strings.TrimLeft(line, " \t\r\f\v\n")
	if strings.HasPrefix(stripped, "#") || stripped == "" {
		return "", "", false
	}
	m := configKeyRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	return m[2], m[4], true
}

func (cf *ConfigFile) IndicesOf(name string) []int {
	var out []int
	for i := range cf.Lines {
		k, _, ok := cf.keyAt(i)
		if ok && k == name {
			out = append(out, i)
		}
	}
	return out
}

func (cf *ConfigFile) GetValues(name string) []string {
	var vals []string
	for _, i := range cf.IndicesOf(name) {
		_, v, ok := cf.keyAt(i)
		if ok {
			vals = append(vals, unquote(v))
		}
	}
	return vals
}

// GetValue returns the last value for name (later lines win in Ghostty).
func (cf *ConfigFile) GetValue(name string) (string, bool) {
	vals := cf.GetValues(name)
	if len(vals) == 0 {
		return "", false
	}
	return vals[len(vals)-1], true
}

// formatLine renders one `name = value` line. quoteStyle '"' forces double
// quotes; 0 auto-quotes only when the value contains whitespace.
func formatLine(name, value string, quoteStyle rune) string {
	v := value
	needsQuote := strings.ContainsAny(value, " \t")
	if quoteStyle == '"' || (quoteStyle == 0 && needsQuote) {
		v = `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return name + " = " + v
}

// SetScalar sets one value, replacing the last occurrence in place and
// commenting out earlier duplicates as superseded.
func (cf *ConfigFile) SetScalar(name, value string) {
	idxs := cf.IndicesOf(name)
	if len(idxs) > 0 {
		i := idxs[len(idxs)-1]
		old := cf.Lines[i]
		var quote rune
		if m := configKeyRe.FindStringSubmatch(old); m != nil && strings.HasPrefix(m[4], `"`) {
			quote = '"'
		}
		cf.Lines[i] = formatLine(name, value, quote)
		for _, j := range idxs[:len(idxs)-1] {
			cf.Lines[j] = "# " + cf.Lines[j] + "  # (superseded)"
		}
		return
	}
	cf.appendManaged([]string{formatLine(name, value, 0)})
}

// SetList replaces every occurrence of a repeated key, inserting the new
// lines where the first occurrence was.
func (cf *ConfigFile) SetList(name string, values []string) {
	idxs := cf.IndicesOf(name)
	var newLines []string
	for _, v := range values {
		newLines = append(newLines, formatLine(name, v, 0))
	}
	if len(idxs) > 0 {
		keep := make(map[int]bool, len(idxs))
		for _, i := range idxs {
			keep[i] = true
		}
		var rebuilt []string
		inserted := false
		for i, line := range cf.Lines {
			if keep[i] {
				if !inserted {
					rebuilt = append(rebuilt, newLines...)
					inserted = true
				}
				continue
			}
			rebuilt = append(rebuilt, line)
		}
		cf.Lines = rebuilt
		return
	}
	if len(newLines) > 0 {
		cf.appendManaged(newLines)
	}
}

// Unset comments out every occurrence of name.
func (cf *ConfigFile) Unset(name string) {
	for _, i := range cf.IndicesOf(name) {
		cf.Lines[i] = "# " + cf.Lines[i] + "  # (removed)"
	}
}

// appendManaged appends lines under the managed section header, creating the
// header (with a separating blank line) if neither it nor a legacy header is
// present yet.
func (cf *ConfigFile) appendManaged(newLines []string) {
	found := false
	for _, l := range cf.Lines {
		if l == managedHeader {
			found = true
			break
		}
		for _, h := range legacyHeaders {
			if l == h {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		if len(cf.Lines) > 0 && strings.TrimSpace(cf.Lines[len(cf.Lines)-1]) != "" {
			cf.Lines = append(cf.Lines, "")
		}
		cf.Lines = append(cf.Lines, managedHeader)
	}
	cf.Lines = append(cf.Lines, newLines...)
}

func (cf *ConfigFile) Render() string {
	return strings.Join(cf.Lines, "\n")
}

// Write renders and writes the file.
func (cf *ConfigFile) Write() error {
	return cf.WriteText(cf.Render())
}

// WriteText writes text, ensuring a trailing newline and parent dirs.
func (cf *ConfigFile) WriteText(text string) error {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if dir := filepath.Dir(cf.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(cf.Path, []byte(text), 0o644)
}

// Backup makes at most one backup per day (a safety net; the TUI also keeps
// an in-memory original for its own revert). Returns "" when there is no
// config file to back up.
func (cf *ConfigFile) Backup() string {
	if !isFile(cf.Path) {
		return ""
	}
	dst := fmt.Sprintf("%s.spookiui.%s.bak", cf.Path, time.Now().Format("20060102"))
	if !fileExists(dst) {
		data, err := os.ReadFile(cf.Path)
		if err != nil {
			return ""
		}
		mode := os.FileMode(0o644)
		if st, err := os.Stat(cf.Path); err == nil {
			mode = st.Mode()
		}
		//nolint:errcheck // best-effort daily backup; failure just means no backup today
		_ = os.WriteFile(dst, data, mode)
	}
	return dst
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return strings.ReplaceAll(v[1:len(v)-1], `\"`, `"`)
	}
	return v
}

// ---- Validation & live reload ----

// validateConfig validates a full config file text with the ghostty binary.
// Returns (ok, error_lines).
func validateConfig(text string) (bool, []string) {
	if ghosttyPath == "" {
		return true, nil
	}
	tmp, err := os.CreateTemp("", "*.spookiui.cfg")
	if err != nil {
		return false, []string{err.Error()}
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, werr := tmp.WriteString(text); werr != nil {
		tmp.Close()
		return false, []string{werr.Error()}
	}
	if cerr := tmp.Close(); cerr != nil {
		return false, []string{cerr.Error()}
	}
	proc, err := runCmd([]string{ghosttyPath, "+validate-config", "--config-file=" + tmpPath}, 30*time.Second)
	if err != nil {
		return false, []string{err.Error()}
	}
	out := strings.TrimSpace(proc.stdout + "\n" + proc.stderr)
	if proc.code == 0 && out == "" {
		return true, nil
	}
	var errs []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.ReplaceAll(line, tmpPath, "")
		line = strings.TrimLeft(line, ": ")
		errs = append(errs, line)
	}
	return proc.code == 0 && len(errs) == 0, errs
}

// reloadGhostty triggers Ghostty to reload its configuration (live): on
// macOS by clicking the "Reload Configuration" menu item via AppleScript, on
// Linux by sending SIGUSR2.
func reloadGhostty() (bool, string) {
	if isMacOS {
		return reloadMacOS()
	}
	if isLinux {
		return reloadLinux()
	}
	return false, "auto-reload not supported on this platform; press your reload_config keybind"
}

func reloadMacOS() (bool, string) {
	script := `tell application "System Events" to tell process "Ghostty" to ` +
		`click menu item "Reload Configuration" of menu 1 of ` +
		`menu bar item "Ghostty" of menu bar 1`
	proc, err := runCmd([]string{"osascript", "-e", script}, 10*time.Second)
	if err != nil {
		return false, err.Error()
	}
	if proc.code == 0 {
		return true, "reloaded"
	}
	msg := strings.TrimSpace(proc.stderr)
	if msg == "" {
		msg = "reload failed"
	}
	if strings.Contains(msg, "not allowed assistive") || strings.Contains(msg, "1002") ||
		strings.Contains(msg, "osascript is not allowed") {
		msg = "needs Accessibility permission: System Settings → Privacy & " +
			"Security → Accessibility → enable your terminal"
	} else if strings.Contains(msg, "Ghostty") && strings.Contains(msg, "process") {
		msg = "Ghostty doesn't appear to be running"
	}
	return false, msg
}

// ghosttyPIDs lists PIDs of running Ghostty processes (best effort).
// A variable so tests can substitute.
var ghosttyPIDs = func() []int {
	var pids []int
	seen := map[int]bool{}
	for _, args := range [][]string{{"pgrep", "-x", "ghostty"}, {"pgrep", "-if", "ghostty"}} {
		proc, err := runCmd(args, 5*time.Second)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(proc.stdout) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			if pid != os.Getpid() && !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
		if len(pids) > 0 {
			break
		}
	}
	return pids
}

// reloadLinux: Ghostty reloads its config on SIGUSR2. Signal every instance.
func reloadLinux() (bool, string) {
	pids := ghosttyPIDs()
	if len(pids) == 0 {
		return false, "Ghostty doesn't appear to be running"
	}
	sent := 0
	for _, pid := range pids {
		err := killUSR2(pid)
		if err == nil {
			sent++
			continue
		}
		if errors.Is(err, errProcessNotFound) {
			continue
		}
		if errors.Is(err, errPermission) {
			return false, fmt.Sprintf("not permitted to signal Ghostty (pid %d)", pid)
		}
		return false, err.Error()
	}
	if sent > 0 {
		return true, "reloaded"
	}
	return false, "Ghostty doesn't appear to be running"
}

func isGhosttyRunning() bool {
	proc, err := runCmd([]string{"pgrep", "-x", "ghostty"}, 5*time.Second)
	if err == nil && strings.TrimSpace(proc.stdout) != "" {
		return true
	}
	proc, err = runCmd([]string{"pgrep", "-if", "Ghostty.app"}, 5*time.Second)
	return err == nil && strings.TrimSpace(proc.stdout) != ""
}

// ---- Listings (themes, fonts, actions, keybinds) ----

func listThemes() []string {
	if ghosttyPath == "" {
		return nil
	}
	proc, err := runCmd([]string{ghosttyPath, "+list-themes", "--plain"}, 30*time.Second)
	if err != nil {
		return nil // ghostty failed to run; treat as no themes
	}
	out := proc.stdout
	if out == "" {
		out = proc.stderr
	}
	var themes []string
	suffixRe := regexp.MustCompile(`\s*\((resources|user)\)\s*$`)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		themes = append(themes, suffixRe.ReplaceAllString(line, ""))
	}
	return themes
}

func listFonts() []string {
	if ghosttyPath == "" {
		return nil
	}
	proc, err := runCmd([]string{ghosttyPath, "+list-fonts"}, 45*time.Second)
	if err != nil {
		return nil // ghostty failed to run; treat as no fonts
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(proc.stdout, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		fam := strings.TrimSpace(line)
		if fam == "" || seen[fam] {
			continue
		}
		seen[fam] = true
		out = append(out, fam)
	}
	return out
}

var actionsCache []string

var actionRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// listActions returns the keybind action names Ghostty knows about.
func listActions() []string {
	if actionsCache != nil {
		return actionsCache
	}
	seen := map[string]bool{}
	var out []string
	if ghosttyPath != "" {
		proc, err := runCmd([]string{ghosttyPath, "+list-actions"}, 20*time.Second)
		if err == nil {
			out2 := proc.stdout
			if out2 == "" {
				out2 = proc.stderr
			}
			for _, line := range strings.Split(out2, "\n") {
				line = strings.TrimSpace(line)
				if actionRe.MatchString(line) && !seen[line] {
					seen[line] = true
					out = append(out, line)
				}
			}
		}
	}
	actionsCache = out
	return out
}

var keybindMods = []string{"super", "ctrl", "alt", "shift"}

var keybindModAliases = map[string]string{
	"cmd": "super", "command": "super", "control": "ctrl",
	"opt": "alt", "option": "alt", "meta": "alt",
}

var keybindNamedKeys = []string{
	"space", "enter", "tab", "escape", "backspace", "delete", "insert",
	"home", "end", "page_up", "page_down", "up", "down", "left", "right",
	"f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12",
	"minus", "equal", "plus", "comma", "period", "slash", "backslash",
	"semicolon", "apostrophe", "grave_accent", "left_bracket", "right_bracket",
}

var defaultKeybindsCache map[string]string

// splitKeybind splits a `trigger=action` keybind. Actions never contain `=`,
// so the last `=` is the separator — this handles triggers that include the
// `=` key (e.g. `super+==increase_font_size`).
func splitKeybind(entry string) (string, string) {
	i := strings.LastIndex(entry, "=")
	if i < 0 {
		return entry, ""
	}
	return entry[:i], entry[i+1:]
}

// listDefaultKeybinds returns Ghostty's built-in keybinds as
// {trigger: action} (for conflict checks).
func listDefaultKeybinds() map[string]string {
	if defaultKeybindsCache != nil {
		return defaultKeybindsCache
	}
	res := map[string]string{}
	if ghosttyPath != "" {
		proc, err := runCmd([]string{ghosttyPath, "+list-keybinds"}, 20*time.Second)
		if err == nil {
			for _, line := range strings.Split(proc.stdout, "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "keybind") {
					continue
				}
				_, rhs, found := strings.Cut(line, "=")
				if !found {
					continue
				}
				trig, act := splitKeybind(strings.TrimSpace(rhs))
				if trig != "" {
					res[trig] = act
				}
			}
		}
	}
	defaultKeybindsCache = res
	return res
}

// ---- Theme files ----

// ghosttyResourcesDir is the best-effort path to the Ghostty resources dir
// (contains `themes/`).
func ghosttyResourcesDir() string {
	if d := os.Getenv("GHOSTTY_RESOURCES_DIR"); d != "" && isDir(d) {
		return d
	}
	var cands []string
	if isMacOS {
		cands = append(cands,
			"/Applications/Ghostty.app/Contents/Resources/ghostty",
			filepath.Join(homeDir(), "Applications/Ghostty.app/Contents/Resources/ghostty"))
	}
	if ghosttyPath != "" {
		if rp, err := filepath.EvalSymlinks(ghosttyPath); err == nil {
			prefix := filepath.Dir(filepath.Dir(rp))
			cands = append(cands, filepath.Join(prefix, "share", "ghostty"))
		}
	}
	cands = append(cands, "/usr/share/ghostty", "/usr/local/share/ghostty",
		"/opt/homebrew/share/ghostty")
	for _, c := range cands {
		if c != "" && isDir(c) {
			return c
		}
	}
	return ""
}

func themeSearchDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, filepath.Join(xdg, "ghostty", "themes"))
	}
	dirs = append(dirs, filepath.Join(homeDir(), ".config", "ghostty", "themes"))
	if isMacOS {
		dirs = append(dirs, filepath.Join(homeDir(),
			"Library", "Application Support", "com.mitchellh.ghostty", "themes"))
	}
	if rd := ghosttyResourcesDir(); rd != "" {
		dirs = append(dirs, filepath.Join(rd, "themes"))
	}
	return dirs
}

func findThemeFile(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, d := range themeSearchDirs() {
		p := filepath.Join(d, name)
		if isFile(p) {
			return p
		}
	}
	return ""
}

// themeVariantName: a theme value may be composite (`light:A,dark:B`); pick
// one name to preview, preferring the dark variant.
func themeVariantName(value string) string {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, ",") && !strings.Contains(value, ":") {
		return value
	}
	picks := map[string]string{}
	first := ""
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, ":") {
			k, v, _ := strings.Cut(part, ":")
			picks[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		} else if first == "" {
			first = part
		}
	}
	if v := picks["dark"]; v != "" {
		return v
	}
	if v := picks["light"]; v != "" {
		return v
	}
	if first != "" {
		return first
	}
	return value
}

// themeColors is a parsed theme file.
type themeColors struct {
	Palette    map[int]string
	Foreground string
	Background string
	Cursor     string
}

var themeColorCache = map[string]*themeColors{}

// parseThemeColors parses a theme file into palette/foreground/background/
// cursor colors. Returns nil when the theme cannot be found or read.
func parseThemeColors(name string) *themeColors {
	if res, ok := themeColorCache[name]; ok {
		return res
	}
	var res *themeColors
	if path := findThemeFile(name); path != "" {
		if data, err := os.ReadFile(path); err == nil {
			res = &themeColors{Palette: map[int]string{}}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
					continue
				}
				key, val, _ := strings.Cut(line, "=")
				key, val = strings.TrimSpace(key), strings.TrimSpace(val)
				switch key {
				case "palette":
					idx, col, _ := strings.Cut(val, "=")
					if i, err := strconv.Atoi(strings.TrimSpace(idx)); err == nil {
						res.Palette[i] = strings.TrimSpace(col)
					}
				case "background":
					res.Background = val
				case "foreground":
					res.Foreground = val
				case "cursor-color":
					res.Cursor = val
				}
			}
		}
	}
	themeColorCache[name] = res
	return res
}

// ---- Color math ----

var hexRe = regexp.MustCompile(`^#?([0-9a-fA-F]{6})$`)

// parseHex parses "#rrggbb" (or "rrggbb") into r, g, b.
func parseHex(value string) (int, int, int, bool) {
	m := hexRe.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return 0, 0, 0, false
	}
	h := m[1]
	// hexRe guarantees two hex digits, so ParseUint cannot fail here.
	hexByte := func(s string) int {
		v, err := strconv.ParseUint(s, 16, 0)
		if err != nil {
			return 0
		}
		return int(v)
	}
	return hexByte(h[0:2]), hexByte(h[2:4]), hexByte(h[4:6]), true
}

// rgbTo256 maps an RGB triple to the nearest xterm-256 color index.
func rgbTo256(r, g, b int) int {
	to6 := func(v int) int {
		if v < 48 {
			return 0
		}
		if v < 115 {
			return 1
		}
		return (v - 35) / 40
	}
	ri, gi, bi := to6(r), to6(g), to6(b)
	ci := 16 + 36*ri + 6*gi + bi
	avg := (r + g + b) / 3
	var gray int
	switch {
	case avg < 8:
		gray = 232
	case avg > 238:
		gray = 255
	default:
		gray = 232 + (avg-8)/10
	}
	cubeVal := func(i int) int {
		if i == 0 {
			return 0
		}
		return 55 + i*40
	}
	cr, cg, cb := cubeVal(ri), cubeVal(gi), cubeVal(bi)
	gl := 8 + (gray-232)*10
	dCube := (cr-r)*(cr-r) + (cg-g)*(cg-g) + (cb-b)*(cb-b)
	dGray := (gl-r)*(gl-r) + (gl-g)*(gl-g) + (gl-b)*(gl-b)
	if dCube <= dGray {
		return ci
	}
	return gray
}

// ---- Signals (Linux reload) ----

var (
	errProcessNotFound = errors.New("process not found")
	errPermission      = errors.New("permission denied")
)

// killUSR2 sends SIGUSR2, mapping ESRCH/EPERM onto sentinel errors.
func killUSR2(pid int) error {
	err := syscall.Kill(pid, syscall.SIGUSR2)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return errProcessNotFound
	}
	if errors.Is(err, syscall.EPERM) {
		return errPermission
	}
	return err
}

// ---- Session: staged edits, apply/revert, profiles ----

type Session struct {
	Schema       map[string]*Option
	Cfg          *ConfigFile
	OriginalText string
	BackupPath   string
	AutoApply    bool
	Dirty        bool
}

func NewSession() (*Session, error) {
	schema, err := loadSchema()
	if err != nil {
		return nil, err
	}
	cfg := NewConfigFile(configPath())
	return &Session{
		Schema:       schema,
		Cfg:          cfg,
		OriginalText: cfg.Render(),
		AutoApply:    true,
	}, nil
}

func (s *Session) ensureBackup() {
	if s.BackupPath == "" {
		s.BackupPath = s.Cfg.Backup()
	}
}

// Effective returns the current effective value (user override or default).
func (s *Session) Effective(name string) string {
	if val, ok := s.Cfg.GetValue(name); ok {
		return val
	}
	if opt := s.Schema[name]; opt != nil {
		return opt.Default
	}
	return ""
}

func (s *Session) EffectiveList(name string) []string {
	if vals := s.Cfg.GetValues(name); len(vals) > 0 {
		return vals
	}
	if opt := s.Schema[name]; opt != nil {
		return append([]string(nil), opt.Defaults...)
	}
	return nil
}

func (s *Session) IsOverridden(name string) bool {
	if len(s.Cfg.IndicesOf(name)) == 0 {
		return false
	}
	opt := s.Schema[name]
	if opt == nil {
		return true
	}
	if opt.IsList {
		return !stringSlicesEqual(s.EffectiveList(name), opt.Defaults)
	}
	return s.Effective(name) != opt.Default
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Session) StageScalar(name, value string) {
	s.Cfg.SetScalar(name, value)
	s.Dirty = true
}

func (s *Session) StageList(name string, values []string) {
	s.Cfg.SetList(name, values)
	s.Dirty = true
}

func (s *Session) StageUnset(name string) {
	s.Cfg.Unset(name)
	s.Dirty = true
}

// Apply validates + writes + reloads. Returns (ok, message).
func (s *Session) Apply() (bool, string) {
	text := s.Cfg.Render()
	ok, errs := validateConfig(text)
	if !ok {
		msg := "validation failed"
		if len(errs) > 0 {
			msg = errs[0]
		}
		return false, "invalid: " + msg
	}
	s.ensureBackup()
	if err := s.Cfg.WriteText(text); err != nil {
		return false, "could not write config: " + err.Error()
	}
	s.Dirty = false
	if s.AutoApply {
		rOk, msg := reloadGhostty()
		if rOk {
			return true, "saved + reloaded live"
		}
		return true, "saved (reload: " + msg + ")"
	}
	return true, "saved (auto-apply off — press 's'/reload to apply live)"
}

func (s *Session) RevertAll() (bool, string) {
	s.Cfg.Lines = strings.Split(s.OriginalText, "\n")
	s.Dirty = false
	if err := s.Cfg.WriteText(s.OriginalText); err != nil {
		return false, "could not write config: " + err.Error()
	}
	if s.AutoApply {
		reloadGhostty()
	}
	return true, "reverted to session start"
}

// RestoreDefaults clears the config file entirely so Ghostty falls back to
// every one of its built-in defaults. Follows the same validate → back up →
// write → reload → rollback discipline as every other mutation path.
func (s *Session) RestoreDefaults() (bool, string) {
	snap := append([]string(nil), s.Cfg.Lines...)
	blank := managedHeader + "\n" +
		"# Configuration reset to Ghostty defaults by SpookiUI.\n"
	ok, errs := validateConfig(blank)
	if !ok {
		s.Cfg.Lines = snap
		msg := "validation failed"
		if len(errs) > 0 {
			msg = errs[0]
		}
		return false, "invalid: " + msg
	}
	s.ensureBackup()
	s.Cfg.Lines = strings.Split(strings.TrimRight(blank, "\n"), "\n")
	if err := s.Cfg.WriteText(blank); err != nil {
		return false, "could not write config: " + err.Error()
	}
	s.Dirty = false
	if s.AutoApply && canReload {
		rOk, msg := reloadGhostty()
		if rOk {
			return true, "restored Ghostty defaults + reloaded live"
		}
		return true, "restored Ghostty defaults (reload: " + msg + ")"
	}
	return true, "restored Ghostty defaults"
}

type override struct {
	Name  string
	Value string
}

// Overrides lists the options the user has changed from default, sorted by
// name (Python iterates the schema in insertion order; Go maps are unordered,
// so we sort for deterministic output).
func (s *Session) Overrides() []override {
	var names []string
	for name := range s.Schema {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []override
	for _, name := range names {
		opt := s.Schema[name]
		if !s.IsOverridden(name) {
			continue
		}
		if opt.IsList {
			v := strings.Join(s.EffectiveList(name), ", ")
			if v == "" {
				v = "(cleared)"
			}
			out = append(out, override{name, v})
		} else {
			out = append(out, override{name, s.Effective(name)})
		}
	}
	return out
}

// SaveProfile snapshots the current config to a named profile.
func (s *Session) SaveProfile(name string) (bool, string) {
	name = strings.TrimSpace(name)
	if !profileNameRe.MatchString(name) {
		return false, "invalid name — use letters, numbers, . _ -"
	}
	text := s.Cfg.Render()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if err := os.MkdirAll(profilesDir(), 0o755); err != nil {
		return false, "could not save profile: " + err.Error()
	}
	if err := os.WriteFile(profilePath(name), []byte(text), 0o644); err != nil {
		return false, "could not save profile: " + err.Error()
	}
	return true, fmt.Sprintf("saved profile '%s'", name)
}

// LoadProfile applies a named profile: validate, back up, write, reload,
// rollback on failure — the same discipline as every other mutation path.
func (s *Session) LoadProfile(name string) (bool, string) {
	path := profilePath(name)
	if !isFile(path) {
		return false, fmt.Sprintf("no profile named '%s'", name)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "could not read profile: " + err.Error()
	}
	text := string(data)
	ok, errs := validateConfig(text)
	if !ok {
		msg := "validation failed"
		if len(errs) > 0 {
			msg = errs[0]
		}
		return false, "profile invalid: " + msg
	}
	s.ensureBackup()
	s.Cfg.Lines = strings.Split(text, "\n")
	if err := s.Cfg.WriteText(text); err != nil {
		return false, "could not write config: " + err.Error()
	}
	s.Dirty = false
	if s.AutoApply && canReload {
		rOk, msg := reloadGhostty()
		if rOk {
			return true, fmt.Sprintf("loaded profile '%s'", name)
		}
		return true, fmt.Sprintf("loaded profile '%s' (reload: %s)", name, msg)
	}
	return true, fmt.Sprintf("loaded profile '%s'", name)
}

func (s *Session) DeleteProfile(name string) (bool, string) {
	path := profilePath(name)
	if !isFile(path) {
		return false, fmt.Sprintf("no profile named '%s'", name)
	}
	if err := os.Remove(path); err != nil {
		return false, "could not delete profile: " + err.Error()
	}
	return true, fmt.Sprintf("deleted profile '%s'", name)
}

// ToggleLightDark flips between the profiles named 'light' and 'dark'.
func (s *Session) ToggleLightDark() (bool, string) {
	if !isFile(profilePath("light")) || !isFile(profilePath("dark")) {
		return false, "save profiles named 'light' and 'dark' first"
	}
	var darkText string
	if data, err := os.ReadFile(profilePath("dark")); err == nil {
		darkText = strings.TrimSpace(string(data))
	}
	target := "dark"
	if strings.TrimSpace(s.Cfg.Render()) == darkText {
		target = "light"
	}
	return s.LoadProfile(target)
}

// ---- Profiles & icon helpers ----

// spookiuiDataDir is SpookiUI's own data dir (cross-platform).
func spookiuiDataDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = filepath.Join(homeDir(), ".local", "share")
	}
	return filepath.Join(base, "spookiui")
}

// profilesDir is where named config snapshots live, outside Ghostty's own
// config dir.
func profilesDir() string {
	return filepath.Join(spookiuiDataDir(), "profiles")
}

var profileNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func profilePath(name string) string {
	return filepath.Join(profilesDir(), name)
}

func listProfiles() []string {
	entries, err := os.ReadDir(profilesDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && profileNameRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// iconsAvailable decides whether to show Nerd Font category icons. We can't
// ask the terminal if a glyph will render, so we key off the strongest
// signal we have: the terminal (Ghostty) font. SPOOKIUI_ICONS=1/0 forces it.
func iconsAvailable(sess *Session) bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("SPOOKIUI_ICONS")))
	switch env {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	for _, fam := range sess.EffectiveList("font-family") {
		low := strings.ToLower(fam)
		if strings.Contains(low, "nerd font") || strings.Contains(low, "nerdfont") {
			return true
		}
	}
	return false
}

func iconNoticeMarker() string {
	return filepath.Join(spookiuiDataDir(), "icon-notice-shown")
}

// iconNoticeText is platform-specific guidance for enabling Nerd Font icons.
func iconNoticeText() string {
	var install string
	switch {
	case isMacOS:
		install = "  macOS:  brew install --cask font-symbols-only-nerd-font\n" +
			"          (or a full one, e.g. font-jetbrains-mono-nerd-font)"
	case isLinux:
		install = "  Linux:  install a Nerd Font from https://www.nerdfonts.com\n" +
			"          (or your distro's package), then run `fc-cache -f`"
	default:
		install = "  Install a Nerd Font from https://www.nerdfonts.com"
	}
	return "SpookiUI can show an icon beside each category, but that needs a Nerd Font\n" +
		"as your terminal font. None is set, so icons are off for now.\n\n" +
		"To enable them:\n" +
		install + "\n" +
		"  Then set it in SpookiUI: open the \"Font\" category and set font-family\n" +
		"  to your Nerd Font (e.g. \"JetBrainsMono Nerd Font\").\n" +
		"  Or set SPOOKIUI_ICONS=1 to force icons on.\n"
}

// maybeShowIconNotice tells the user once, before entering the TUI, how to
// enable category icons. Never blocks the app — on any hiccup we just
// continue into the fallback view. (Used by the TUI phase.)
func maybeShowIconNotice(out io.Writer, in io.Reader) {
	marker := iconNoticeMarker()
	if fileExists(marker) {
		return
	}
	fmt.Fprint(out, iconNoticeText())
	fmt.Fprint(out, "Press Enter to continue… ")
	// Best-effort read of one line; any failure just continues.
	var line string
	//nolint:errcheck // EOF or a bare Enter is fine — nothing to read, carry on
	_, _ = fmt.Fscanln(in, &line)
	if err := os.MkdirAll(spookiuiDataDir(), 0o755); err != nil {
		return
	}
	//nolint:errcheck // best-effort marker write; the notice may simply show again
	_ = os.WriteFile(marker, []byte("shown\n"), 0o644)
}

// ---- Doctor: config health check ----

type finding struct {
	Severity string // error / warn / info / ok
	Message  string
}

// runDoctor health-checks the config, most serious findings first.
func runDoctor(sess *Session) []finding {
	cfg, schema := sess.Cfg, sess.Schema
	var errors_, warns, infos []finding

	if ok, errs := validateConfig(cfg.Render()); !ok {
		for _, e := range errs {
			errors_ = append(errors_, finding{"error", "invalid config: " + e})
		}
	}

	present := map[string][]int{}
	for i := range cfg.Lines {
		if k, _, ok := cfg.keyAt(i); ok {
			present[k] = append(present[k], i)
		}
	}
	var presentNames []string
	for name := range present {
		presentNames = append(presentNames, name)
	}
	sort.Strings(presentNames)

	for _, name := range presentNames {
		if schema[name] == nil {
			warns = append(warns, finding{"warn", fmt.Sprintf(
				"unknown option `%s` — not recognised by your Ghostty (a typo or removed option?)", name)})
		}
	}
	for _, name := range presentNames {
		opt := schema[name]
		if opt != nil && !opt.IsList && len(present[name]) > 1 {
			warns = append(warns, finding{"warn", fmt.Sprintf(
				"`%s` is set %d× — only the last takes effect; the earlier ones are dead",
				name, len(present[name]))})
		}
	}
	for _, name := range presentNames {
		if schema[name] != nil && !sess.IsOverridden(name) {
			infos = append(infos, finding{"info", fmt.Sprintf(
				"`%s` is set to its default — redundant, can be removed", name)})
		}
	}

	triggers := map[string][]string{}
	var trigOrder []string
	for _, entry := range cfg.GetValues("keybind") {
		trig, _ := splitKeybind(entry)
		if _, seen := triggers[trig]; !seen {
			trigOrder = append(trigOrder, trig)
		}
		triggers[trig] = append(triggers[trig], entry)
	}
	for _, trig := range trigOrder {
		entries := triggers[trig]
		if len(entries) > 1 {
			warns = append(warns, finding{"warn", fmt.Sprintf(
				"keybind trigger `%s` is bound %d× — the later binding shadows the earlier",
				trig, len(entries))})
		}
	}
	defaults := listDefaultKeybinds()
	for _, trig := range trigOrder {
		entries := triggers[trig]
		if defAct, ok := defaults[trig]; ok {
			_, act := splitKeybind(entries[len(entries)-1])
			if act != "" && act != defAct {
				infos = append(infos, finding{"info", fmt.Sprintf(
					"keybind `%s` overrides Ghostty's default (default is `%s`)", trig, defAct)})
			}
		}
	}

	findings := append(append(errors_, warns...), infos...)
	if len(findings) == 0 {
		findings = []finding{{"ok", "no issues found — config looks healthy"}}
	}
	return findings
}

// ---- Utils: SSH terminfo fix ----
//
// Ghostty advertises itself to programs with TERM=xterm-ghostty. When you SSH
// into another host, that host looks "xterm-ghostty" up in *its own* terminfo
// database — and most remote boxes have never heard of it. The remote shell
// then misbehaves: garbled or dead keys, no colour, broken `clear`/`tput`, or
// the classic `Error opening terminal: xterm-ghostty`. Forcing the `ssh`
// command to use a TERM every host already ships (xterm-256color) sidesteps
// this without touching the remote. The alias below does exactly that.

const sshAliasLine = `alias ssh="TERM=xterm-256color ssh"`
const sshFixMarker = "# added by SpookiUI — force a portable TERM over SSH (see fix-ssh)"

var sshAliasRe = regexp.MustCompile(`^\s*alias\s+ssh\s*=\s*['"]?\s*TERM=xterm-256color\s+ssh`)

var sshFixExplanation = []string{
	"Fix SSH — terminfo over SSH",
	"",
	"Ghostty tells programs it is `xterm-ghostty` (via the TERM variable).",
	"When you SSH into another machine, that machine looks xterm-ghostty up",
	"in its own terminfo database — and most remote hosts have never heard",
	"of it. The remote shell then misbehaves: garbled or dead keys, missing",
	"colour, broken `clear`/`tput`, or the classic error:",
	"    Error opening terminal: xterm-ghostty",
	"",
	"What this does",
	"  Adds one line to your shell rc (~/.zshrc or ~/.bashrc):",
	"      " + sshAliasLine,
	"  so the `ssh` command runs with TERM=xterm-256color — a terminfo entry",
	"  essentially every host already ships. Your local Ghostty session keeps",
	"  its full xterm-ghostty features; only the outbound SSH connection is",
	"  downgraded to the universally-understood xterm-256color.",
	"",
	"Safe & idempotent",
	"  If the alias is already present it does nothing. Nothing on the remote",
	"  host is changed. To undo, delete the alias line from your rc file.",
	"  (A more thorough alternative is copying Ghostty's terminfo to each host,",
	"   but this alias is the quick fix that needs no remote access.)",
}

// sshRCScanFiles lists existing shell rc files that might already carry the
// ssh alias.
func sshRCScanFiles() []string {
	names := []string{".zshrc", ".bashrc", ".bash_profile", ".zprofile",
		".profile", ".bash_aliases"}
	var out []string
	for _, n := range names {
		p := filepath.Join(homeDir(), n)
		if isFile(p) {
			out = append(out, p)
		}
	}
	return out
}

// sshRCTarget picks which rc file to add the alias to: the current login
// shell's primary rc. Defaults to ~/.zshrc (the macOS/Ghostty default shell).
func sshRCTarget() string {
	shell := filepath.Base(os.Getenv("SHELL"))
	home := homeDir()
	if shell == "bash" {
		for _, n := range []string{".bashrc", ".bash_profile"} {
			p := filepath.Join(home, n)
			if fileExists(p) {
				return p
			}
		}
		return filepath.Join(home, ".bashrc")
	}
	return filepath.Join(home, ".zshrc")
}

// findSSHAlias returns the path of the first shell rc that already defines
// the TERM ssh alias.
func findSSHAlias() string {
	for _, path := range sshRCScanFiles() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if sshAliasRe.MatchString(line) {
				return path
			}
		}
	}
	return ""
}

// verifyRC syntax-checks the rc with the shell's `-n` flag rather than fully
// sourcing it: a child process cannot change the parent shell's environment
// anyway, and fully executing someone's rc non-interactively can hang.
func verifyRC(path string) (bool, string) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	proc, err := runCmd([]string{shell, "-n", path}, 10*time.Second)
	if err != nil {
		return false, err.Error()
	}
	if proc.code == 0 {
		return true, "rc parses cleanly"
	}
	out := strings.TrimSpace(proc.stderr)
	if out == "" {
		out = strings.TrimSpace(proc.stdout)
	}
	if out == "" {
		out = "shell reported an error"
	}
	return false, out
}

// applySSHFix adds the ssh TERM alias to the user's shell rc if it isn't
// already there. Idempotent.
func applySSHFix() (bool, string) {
	if existing := findSSHAlias(); existing != "" {
		return true, fmt.Sprintf("already fixed — an ssh alias forcing TERM=xterm-256color "+
			"is present in %s; nothing to do", tilde(existing))
	}
	target := sshRCTarget()
	block := "\n" + sshFixMarker + "\n" + sshAliasLine + "\n"
	f, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Sprintf("could not write %s: %v", tilde(target), err)
	}
	if _, err := f.WriteString(block); err != nil {
		f.Close()
		return false, fmt.Sprintf("could not write %s: %v", tilde(target), err)
	}
	f.Close()
	reloadHint := fmt.Sprintf("run `source %s` or open a new terminal for it to take effect now",
		tilde(target))
	ok, note := verifyRC(target)
	if ok {
		return true, fmt.Sprintf("added the ssh alias to %s — %s", tilde(target), reloadHint)
	}
	return true, fmt.Sprintf("added the ssh alias to %s (warning: %s) — %s",
		tilde(target), note, reloadHint)
}

// ---- Command line interface ----

const usageLine = "usage: spookiui [-h] [-V] <command> [args]"

func helpText() string {
	return usageLine + `

Live configurator for the Ghostty terminal. Run with no subcommand to launch
the interactive TUI.

options:
  -h, --help     show this help message and exit
  -V, --version  show the version and exit

commands:
  list      list options (optionally by category); --all includes other OSes
  get       print an option's current value
  doc       show docs for an option
  set       set an option (writes + reloads live); --no-reload skips reload
  version   print the version and check GitHub for updates (--no-check skips)
  update    update SpookiUI in place to the latest release
  reset     restore Ghostty defaults (clears your config file); needs --yes
  reload    trigger Ghostty to reload its config
  validate  validate the current config file
  themes    list available themes
  fonts     list available monospace font families
  path      print the config file path in use
  profile   save/load named config snapshots: save|load|list|delete|toggle|show [name]
  doctor    health-check the config for issues
  fix-ssh   fix garbled SSH sessions (--check, --explain)

exit codes: 0 ok · 1 generic error · 2 usage / unknown key · 3 ghostty not found
`
}

// cliUsageError prints a usage error like argparse and returns exit code 2.
func cliUsageError(stderr io.Writer, format string, args ...any) int {
	fmt.Fprintln(stderr, usageLine)
	fmt.Fprintf(stderr, "spookiui: error: "+format+"\n", args...)
	return 2
}

// parseSubArgs splits subcommand arguments into positionals and boolean
// flags. Anything starting with "-" must be in knownFlags.
func parseSubArgs(args []string, knownFlags ...string) (pos []string, flags map[string]bool, bad string) {
	known := map[string]bool{}
	for _, f := range knownFlags {
		known[f] = true
	}
	flags = map[string]bool{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			if !known[a] {
				return nil, nil, a
			}
			flags[a] = true
			continue
		}
		pos = append(pos, a)
	}
	return pos, flags, ""
}

func cliList(sess *Session, args []string, stdout, stderr io.Writer) int {
	pos, flags, bad := parseSubArgs(args, "--all")
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if len(pos) > 1 {
		return cliUsageError(stderr, "list takes at most one category")
	}
	cats := categoryOrder
	if len(pos) == 1 {
		cats = []string{pos[0]}
	}
	showAll := flags["--all"]
	for _, cat := range cats {
		var names []string
		for name, o := range sess.Schema {
			if o.Category == cat && (showAll || platformVisible(o)) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		if len(names) == 0 {
			continue
		}
		fmt.Fprintf(stdout, "\n== %s ==\n", cat)
		for _, n := range names {
			o := sess.Schema[n]
			mark := " "
			if sess.IsOverridden(n) {
				mark = "*"
			}
			var val string
			if o.IsList {
				val = strings.Join(sess.EffectiveList(n), ", ")
			} else {
				val = sess.Effective(n)
			}
			fmt.Fprintf(stdout, " %s %-34s %-7s %s\n", mark, n, o.Kind, val)
		}
	}
	return 0
}

func cliGet(sess *Session, args []string, stdout, stderr io.Writer) int {
	pos, _, bad := parseSubArgs(args)
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if len(pos) != 1 {
		return cliUsageError(stderr, "get needs exactly one key")
	}
	o := sess.Schema[pos[0]]
	if o == nil {
		fmt.Fprintf(stderr, "unknown option: %s\n", pos[0])
		return 2
	}
	if o.IsList {
		for _, v := range sess.EffectiveList(pos[0]) {
			fmt.Fprintln(stdout, v)
		}
	} else {
		fmt.Fprintln(stdout, sess.Effective(pos[0]))
	}
	return 0
}

func cliDoc(sess *Session, args []string, stdout, stderr io.Writer) int {
	pos, _, bad := parseSubArgs(args)
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if len(pos) != 1 {
		return cliUsageError(stderr, "doc needs exactly one key")
	}
	o := sess.Schema[pos[0]]
	if o == nil {
		fmt.Fprintf(stderr, "unknown option: %s\n", pos[0])
		return 2
	}
	typ := o.Kind
	if o.IsList {
		typ += ", list"
	}
	fmt.Fprintf(stdout, "%s  (type: %s)\n", o.Name, typ)
	dflt := o.Default
	if dflt == "" {
		dflt = "(empty)"
	}
	fmt.Fprintf(stdout, "default: %s\n", dflt)
	if len(o.Values) > 0 {
		fmt.Fprintf(stdout, "choices: %s\n", strings.Join(o.Values, ", "))
	}
	if o.ReloadNote != "" {
		fmt.Fprintf(stdout, "note: %s\n", o.ReloadNote)
	}
	fmt.Fprintln(stdout)
	doc := o.Doc
	if doc == "" {
		doc = "(no documentation)"
	}
	fmt.Fprintln(stdout, doc)
	return 0
}

func cliSet(sess *Session, args []string, stdout, stderr io.Writer) int {
	pos, flags, bad := parseSubArgs(args, "--no-reload")
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if len(pos) < 1 {
		return cliUsageError(stderr, "set needs a key")
	}
	key := pos[0]
	values := pos[1:]
	if len(values) == 0 {
		return cliUsageError(stderr, "set needs at least one value")
	}
	o := sess.Schema[key]
	if o == nil {
		fmt.Fprintf(stderr, "unknown option: %s\n", key)
		return 2
	}
	snap := append([]string(nil), sess.Cfg.Lines...)
	if o.IsList {
		sess.StageList(key, values)
	} else {
		sess.StageScalar(key, values[0])
	}
	ok, errs := validateConfig(sess.Cfg.Render())
	if !ok {
		sess.Cfg.Lines = snap
		msg := "validation failed"
		if len(errs) > 0 {
			msg = errs[0]
		}
		fmt.Fprintf(stderr, "invalid: %s\n", msg)
		return 1
	}
	sess.ensureBackup()
	if err := sess.Cfg.Write(); err != nil {
		fmt.Fprintf(stderr, "could not write config: %v\n", err)
		return 1
	}
	if flags["--no-reload"] {
		fmt.Fprintf(stdout, "%s set · saved (no reload)\n", key)
		return 0
	}
	_, m := reloadGhostty()
	fmt.Fprintf(stdout, "%s set · saved · reload: %s\n", key, m)
	return 0
}

func cliVersion(sess *Session, args []string, stdout, stderr io.Writer) int {
	_, flags, bad := parseSubArgs(args, "--no-check")
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	fmt.Fprintf(stdout, "SpookiUI v%s\n", version)
	if flags["--no-check"] {
		return 0
	}
	info := checkForUpdate(true, nowSeconds())
	if info == nil {
		fmt.Fprintln(stderr, "update check: no published release found (or GitHub unreachable)")
		return 0
	}
	if info.Outdated {
		fmt.Fprintf(stdout, "a newer release is available: %s\n", info.Latest)
		fmt.Fprintf(stdout, "  %s\n", info.URL)
		fmt.Fprintln(stdout, "  run `spookiui update` to upgrade in place")
	} else {
		fmt.Fprintln(stdout, "you're on the latest release")
	}
	return 0
}

func cliUpdate(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	fmt.Fprintf(stdout, "SpookiUI v%s — checking for updates…\n", version)
	ok, msg := selfUpdate(nowSeconds())
	if ok {
		fmt.Fprintln(stdout, msg)
	} else {
		fmt.Fprintln(stderr, msg)
	}
	if ok {
		return 0
	}
	return 1
}

func cliReset(sess *Session, args []string, stdout, stderr io.Writer) int {
	_, flags, bad := parseSubArgs(args, "--yes")
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if !flags["--yes"] {
		fmt.Fprintf(stderr, "This clears your config file and restores every Ghostty default.\n"+
			"A dated backup of %s is kept.\n"+
			"Re-run with --yes to proceed.\n", sess.Cfg.Path)
		return 1
	}
	ok, m := sess.RestoreDefaults()
	fmt.Fprintln(stdout, m)
	if ok {
		return 0
	}
	return 1
}

func cliReload(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	ok, m := reloadGhostty()
	fmt.Fprintln(stdout, m)
	if ok {
		return 0
	}
	return 1
}

func cliValidate(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	ok, errs := validateConfig(sess.Cfg.Render())
	if ok {
		fmt.Fprintln(stdout, "config is valid")
		return 0
	}
	for _, e := range errs {
		fmt.Fprintln(stderr, e)
	}
	return 1
}

func cliThemes(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	for _, t := range listThemes() {
		fmt.Fprintln(stdout, t)
	}
	return 0
}

func cliFonts(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	for _, f := range listFonts() {
		fmt.Fprintln(stdout, f)
	}
	return 0
}

func cliPath(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	fmt.Fprintln(stdout, sess.Cfg.Path)
	return 0
}

var profileActions = []string{"save", "load", "list", "delete", "toggle", "show"}

func cliProfile(sess *Session, args []string, stdout, stderr io.Writer) int {
	pos, _, bad := parseSubArgs(args)
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if len(pos) < 1 {
		return cliUsageError(stderr, "profile needs an action (save, load, list, delete, toggle, show)")
	}
	act := pos[0]
	if !containsString(profileActions, act) {
		return cliUsageError(stderr, "invalid profile action: %s", act)
	}
	var name string
	if len(pos) > 1 {
		name = pos[1]
	}
	switch act {
	case "list":
		ps := listProfiles()
		if len(ps) == 0 {
			fmt.Fprintln(stderr, "no profiles saved")
			return 0
		}
		for _, p := range ps {
			fmt.Fprintln(stdout, p)
		}
		return 0
	case "toggle":
		ok, m := sess.ToggleLightDark()
		if ok {
			fmt.Fprintln(stdout, m)
			return 0
		}
		fmt.Fprintln(stderr, m)
		return 1
	}
	if name == "" {
		fmt.Fprintf(stderr, "'%s' needs a profile name\n", act)
		return 2
	}
	if act == "show" {
		path := profilePath(name)
		if !isFile(path) {
			fmt.Fprintf(stderr, "no profile named '%s'\n", name)
			return 1
		}
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "could not read profile: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, string(data))
		return 0
	}
	var ok bool
	var m string
	switch act {
	case "save":
		ok, m = sess.SaveProfile(name)
	case "load":
		ok, m = sess.LoadProfile(name)
	case "delete":
		ok, m = sess.DeleteProfile(name)
	}
	if ok {
		fmt.Fprintln(stdout, m)
		return 0
	}
	fmt.Fprintln(stderr, m)
	return 1
}

func cliDoctor(sess *Session, args []string, stdout, stderr io.Writer) int {
	if _, _, bad := parseSubArgs(args); bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	findings := runDoctor(sess)
	icons := map[string]string{"error": "✗", "warn": "!", "info": "·", "ok": "✓"}
	nErr, nWarn := 0, 0
	for _, f := range findings {
		switch f.Severity {
		case "error":
			nErr++
		case "warn":
			nWarn++
		}
		icon, ok := icons[f.Severity]
		if !ok {
			icon = " "
		}
		out := stdout
		if f.Severity == "error" {
			out = stderr
		}
		fmt.Fprintf(out, "%s %s\n", icon, f.Message)
	}
	fmt.Fprintf(stdout, "\n%d error(s), %d warning(s)\n", nErr, nWarn)
	if nErr > 0 {
		return 1
	}
	return 0
}

func cliFixSSH(sess *Session, args []string, stdout, stderr io.Writer) int {
	_, flags, bad := parseSubArgs(args, "--check", "--explain")
	if bad != "" {
		return cliUsageError(stderr, "unrecognized flag: %s", bad)
	}
	if flags["--explain"] {
		for _, line := range sshFixExplanation {
			fmt.Fprintln(stdout, line)
		}
		return 0
	}
	if flags["--check"] {
		if existing := findSSHAlias(); existing != "" {
			fmt.Fprintf(stdout, "ssh alias present in %s\n", tilde(existing))
			return 0
		}
		fmt.Fprintf(stderr, "ssh alias not found in any shell rc (would be added to %s)\n",
			tilde(sshRCTarget()))
		return 1
	}
	ok, m := applySSHFix()
	if ok {
		fmt.Fprintln(stdout, m)
		return 0
	}
	fmt.Fprintln(stderr, m)
	return 1
}

// ---- Terminal layer: raw mode, screen control, frames, keys ----
//
// This section replaces Python's curses: termios raw mode via ioctl, the
// alternate screen, a whole-frame renderer (one write per frame), a pure
// escape-sequence key parser feeding a reader goroutine, and signal wiring.
// The TUI phase (runTUI) consumes this layer; it is not wired up yet.

// termioctls returns the get/set termios ioctl request numbers for goos.
// The values are declared locally: syscall exports TIOCGETA/TIOCSETA only on
// darwin, and TCGETS/TCSETS only on some linux architectures.
func termioctls(goos string) (uintptr, uintptr) {
	if goos == "darwin" {
		return 0x40487413, 0x80487414 // TIOCGETA, TIOCSETA
	}
	return 0x5401, 0x5402 // TCGETS, TCSETS
}

// ccMinMax returns the Cc array indices of VMIN and VTIME for goos
// (darwin: 16/17, linux: 6/5; not exported by syscall on linux/amd64).
func ccMinMax(goos string) (int, int) {
	if goos == "darwin" {
		return 16, 17
	}
	return 6, 5
}

// makeRawTermios mirrors curses cbreak()+noecho(): canonical mode, echo and
// extended input processing off; CR/NL mapping, flow control, stripping and
// parity checking off. ISIG stays set so Ctrl-C still raises SIGINT (Python
// relies on KeyboardInterrupt). VMIN=1/VTIME=0 returns one key per read.
func makeRawTermios(orig syscall.Termios, goos string) syscall.Termios {
	t := orig
	t.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.IEXTEN
	t.Iflag &^= syscall.ICRNL | syscall.IXON | syscall.ISTRIP | syscall.INPCK
	vmin, vtime := ccMinMax(goos)
	t.Cc[vmin] = 1
	t.Cc[vtime] = 0
	return t
}

// tcGetAttr reads the termios of fd. The Termios pointer stays live across
// the syscall; keep the unsafe.Pointer use contained in this expression.
func tcGetAttr(fd uintptr) (syscall.Termios, error) {
	var t syscall.Termios
	get, _ := termioctls(runtime.GOOS)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, get, uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return t, errno
	}
	return t, nil
}

// tcSetAttr writes the termios of fd.
func tcSetAttr(fd uintptr, t *syscall.Termios) error {
	_, set := termioctls(runtime.GOOS)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, set, uintptr(unsafe.Pointer(t)))
	if errno != 0 {
		return errno
	}
	return nil
}

// winsize mirrors struct winsize for the TIOCGWINSZ ioctl.
type winsize struct {
	Row, Col, Xpixel, Ypixel uint16
}

// tcGetWinSize returns the terminal size in rows/cols.
func tcGetWinSize(fd uintptr) (int, int, error) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, 0, errno
	}
	return int(ws.Row), int(ws.Col), nil
}

// Term owns the terminal while the TUI runs: raw mode, alternate screen,
// hidden cursor, and the current size (re-polled each frame and after
// SIGWINCH).
type Term struct {
	in       *os.File
	out      *os.File
	saved    *syscall.Termios
	rows     int
	cols     int
	color256 bool
}

// openTerm enters cbreak+noecho raw mode on in, switches out to the
// alternate screen, hides the cursor and clears the first frame.
func openTerm(in, out *os.File) (*Term, error) {
	fd := in.Fd()
	orig, err := tcGetAttr(fd)
	if err != nil {
		return nil, fmt.Errorf("reading termios: %w", err)
	}
	raw := makeRawTermios(orig, runtime.GOOS)
	if err := tcSetAttr(fd, &raw); err != nil {
		return nil, fmt.Errorf("setting raw mode: %w", err)
	}
	t := &Term{
		in:       in,
		out:      out,
		saved:    &orig,
		color256: supports256Color(os.Getenv("TERM"), os.Getenv("COLORTERM")),
	}
	// Alternate screen, hide cursor, clear and home for the first frame.
	if _, err := out.Write([]byte("\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")); err != nil {
		t.Close()
		return nil, fmt.Errorf("entering alternate screen: %w", err)
	}
	t.Size()
	return t, nil
}

// Close shows the cursor, leaves the alternate screen and restores the saved
// termios. Idempotent.
func (t *Term) Close() {
	if t.saved == nil {
		return
	}
	saved := t.saved
	t.saved = nil
	//nolint:errcheck // best-effort restore; nothing useful to do on failure
	_, _ = t.out.Write([]byte("\x1b[?25h\x1b[?1049l"))
	//nolint:errcheck // best-effort restore; nothing useful to do on failure
	_ = tcSetAttr(t.in.Fd(), saved)
}

// Size re-polls TIOCGWINSZ (cheap; call once per frame) and returns
// rows, cols, keeping the last good values if the ioctl fails.
func (t *Term) Size() (int, int) {
	rows, cols, err := tcGetWinSize(t.out.Fd())
	if err == nil && rows > 0 && cols > 0 {
		t.rows, t.cols = rows, cols
	}
	return t.rows, t.cols
}

// WriteFrame writes a rendered frame with a single write.
func (t *Term) WriteFrame(frame []byte) error {
	_, err := t.out.Write(frame)
	return err
}

// Color256 reports whether the terminal advertises 256-color support.
func (t *Term) Color256() bool { return t.color256 }

// supports256Color probes 256-color capability from TERM/COLORTERM values
// (passed in so the probe is unit-testable).
func supports256Color(term, colorTerm string) bool {
	return strings.Contains(term, "256color") ||
		strings.Contains(term, "ghostty") ||
		strings.Contains(term, "kitty") ||
		colorTerm != ""
}

// hexTo256 maps a "#rrggbb" color to its nearest xterm-256 index (used for
// dynamic swatches, which need no curses pair allocation).
func hexTo256(hex string) (int, bool) {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return 0, false
	}
	return rgbTo256(r, g, b), true
}

// textAttr describes the SGR rendition of a span: bold/reverse plus either a
// fixed curses-style color pair (1–10, mirroring App._init_colors) or direct
// 256-color indexes. fg256/bg256 use 0 = unset (the zero value is a plain
// span); direct indexes are 1–255, which is fine because rgbTo256 never
// returns the 0–15 system colors — those come from pairs.
type textAttr struct {
	bold    bool
	reverse bool
	pair    int
	fg256   int
	bg256   int
}

// fixedPairs maps pair number → [fg, bg] curses color (0–7), -1 = default.
// It mirrors App._init_colors (spookiui.py): pair 2/4/5/6/7/8/10 keep the
// terminal default background (use_default_colors), which SGR omits.
var fixedPairs = map[int][2]int{
	1:  {0, 6},  // black on cyan
	2:  {6, -1}, // cyan on default
	3:  {0, 7},  // black on white
	4:  {7, -1}, // white on default
	5:  {3, -1}, // yellow on default
	6:  {2, -1}, // green on default
	7:  {1, -1}, // red on default
	8:  {5, -1}, // magenta on default
	9:  {0, 3},  // black on yellow
	10: {5, -1}, // magenta on default
}

// frameBuffer accumulates one full frame; render() returns it as a byte
// stream so a frame is flushed with a single os.Stdout write.
type frameBuffer struct {
	buf      bytes.Buffer
	rows     int
	cols     int
	cursorOn bool
	cursorY  int
	cursorX  int
}

func newFrameBuffer(rows, cols int) *frameBuffer {
	return &frameBuffer{rows: rows, cols: cols}
}

// addstr is Python's App.safe: absolute cursor positioning, clipped by rune
// count (Python len() semantics; all glyphs used are width-1), keeping the
// last column free like curses does.
func (fb *frameBuffer) addstr(y, x int, text string, a textAttr) {
	if y < 0 || y >= fb.rows || x < 0 || x >= fb.cols {
		return
	}
	text = clipRunes(text, fb.cols-x-1)
	if text == "" {
		return
	}
	fmt.Fprintf(&fb.buf, "\x1b[%d;%dH", y+1, x+1)
	fb.writeAttr(a)
	fb.buf.Write([]byte(text))
}

// writeAttr emits the SGR sequence for a: reset, bold, reverse, then colors
// (pair colors 0–7 → 30+n/40+n, default bg omitted; direct 256 → 38;5;n /
// 48;5;n).
func (fb *frameBuffer) writeAttr(a textAttr) {
	codes := []string{"0"}
	if a.bold {
		codes = append(codes, "1")
	}
	if a.reverse {
		codes = append(codes, "7")
	}
	fg, bg := a.fg256, a.bg256
	fgBasic, bgBasic := false, false
	if p, ok := fixedPairs[a.pair]; ok {
		if fg == 0 {
			fg, fgBasic = p[0], true
		}
		if bg == 0 {
			bg, bgBasic = p[1], true
		}
	}
	if fgBasic {
		if fg >= 0 { // pair fg 0–7; -1 means default → omit
			codes = append(codes, strconv.Itoa(30+fg))
		}
	} else if fg > 0 {
		codes = append(codes, "38;5;"+strconv.Itoa(fg))
	}
	if bgBasic {
		if bg >= 0 {
			codes = append(codes, strconv.Itoa(40+bg))
		}
	} else if bg > 0 {
		codes = append(codes, "48;5;"+strconv.Itoa(bg))
	}
	fb.buf.Write([]byte("\x1b[" + strings.Join(codes, ";") + "m"))
}

// showCursor makes the cursor visible at (y, x) at the end of the frame
// (Python's _prompt_bar shows it at the edit point).
func (fb *frameBuffer) showCursor(y, x int) {
	fb.cursorOn, fb.cursorY, fb.cursorX = true, y, x
}

// render finalizes the frame and returns its bytes.
func (fb *frameBuffer) render() []byte {
	if fb.cursorOn {
		fb.buf.Write([]byte("\x1b[?25h"))
		fmt.Fprintf(&fb.buf, "\x1b[%d;%dH", fb.cursorY+1, fb.cursorX+1)
	}
	return fb.buf.Bytes()
}

// clipRunes truncates s to at most max runes (Python text[:n] semantics).
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// keyKind classifies a decoded key press.
type keyKind int

const (
	keyNone keyKind = iota // incomplete sequence: parser wants more bytes
	keyRune                // printable ASCII; Rune set
	keyUTF8                // non-ASCII rune decoded; Rune set (Python drops these in text fields)
	keyEnter
	keyBackspace
	keyTab
	keyShiftTab
	keyEscape
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyDelete
	keyPgUp
	keyPgDn
	keyCtrlC
	keyCtrlX
	keyUnknown // unrecognized bytes; consumed and dropped
)

func (k keyKind) String() string {
	switch k {
	case keyNone:
		return "none"
	case keyRune:
		return "rune"
	case keyUTF8:
		return "utf8"
	case keyEnter:
		return "enter"
	case keyBackspace:
		return "backspace"
	case keyTab:
		return "tab"
	case keyShiftTab:
		return "shift-tab"
	case keyEscape:
		return "escape"
	case keyUp:
		return "up"
	case keyDown:
		return "down"
	case keyLeft:
		return "left"
	case keyRight:
		return "right"
	case keyHome:
		return "home"
	case keyEnd:
		return "end"
	case keyDelete:
		return "delete"
	case keyPgUp:
		return "pgup"
	case keyPgDn:
		return "pgdn"
	case keyCtrlC:
		return "ctrl-c"
	case keyCtrlX:
		return "ctrl-x"
	case keyUnknown:
		return "unknown"
	}
	return "?"
}

// KeyEvent is one decoded key press.
type KeyEvent struct {
	Kind keyKind
	Rune rune
}

// parseKey decodes the first key in buf, returning the event and how many
// bytes it consumed. An incomplete sequence returns keyNone with consumed 0;
// the caller (keyReader) decides via the ESC timeout whether to wait for more
// bytes or resolve a lone ESC as keyEscape.
func parseKey(buf []byte) (KeyEvent, int) {
	if len(buf) == 0 {
		return KeyEvent{Kind: keyNone}, 0
	}
	b := buf[0]
	if b == 0x1b {
		return parseEscape(buf)
	}
	switch {
	case b == '\r' || b == '\n':
		return KeyEvent{Kind: keyEnter}, 1
	case b == 0x7f || b == 0x08:
		return KeyEvent{Kind: keyBackspace}, 1
	case b == '\t':
		return KeyEvent{Kind: keyTab}, 1
	case b == 0x03:
		return KeyEvent{Kind: keyCtrlC}, 1
	case b == 0x18:
		return KeyEvent{Kind: keyCtrlX}, 1
	case b >= 0x20 && b <= 0x7e:
		return KeyEvent{Kind: keyRune, Rune: rune(b)}, 1
	case b < 0x80:
		return KeyEvent{Kind: keyUnknown}, 1
	}
	// Non-ASCII: decode one UTF-8 rune (flagged; text fields may drop it).
	if !utf8.FullRune(buf) {
		return KeyEvent{Kind: keyNone}, 0
	}
	r, size := utf8.DecodeRune(buf)
	if r == utf8.RuneError && size == 1 {
		return KeyEvent{Kind: keyUnknown}, 1
	}
	return KeyEvent{Kind: keyUTF8, Rune: r}, size
}

// parseEscape decodes CSI (ESC [) and SS3 (ESC O) sequences.
func parseEscape(buf []byte) (KeyEvent, int) {
	if len(buf) < 2 {
		return KeyEvent{Kind: keyNone}, 0
	}
	intro := buf[1]
	if intro != '[' && intro != 'O' {
		return KeyEvent{Kind: keyUnknown}, 2
	}
	if len(buf) < 3 {
		return KeyEvent{Kind: keyNone}, 0
	}
	switch buf[2] {
	case 'A':
		return KeyEvent{Kind: keyUp}, 3
	case 'B':
		return KeyEvent{Kind: keyDown}, 3
	case 'C':
		return KeyEvent{Kind: keyRight}, 3
	case 'D':
		return KeyEvent{Kind: keyLeft}, 3
	case 'H':
		return KeyEvent{Kind: keyHome}, 3
	case 'F':
		return KeyEvent{Kind: keyEnd}, 3
	case 'Z':
		if intro == '[' {
			return KeyEvent{Kind: keyShiftTab}, 3
		}
		return KeyEvent{Kind: keyUnknown}, 3
	}
	if intro == 'O' || buf[2] < '0' || buf[2] > '9' {
		return KeyEvent{Kind: keyUnknown}, 3
	}
	// CSI with a parameter digit: expect a '~' final byte.
	if len(buf) < 4 {
		return KeyEvent{Kind: keyNone}, 0
	}
	if buf[3] != '~' {
		return KeyEvent{Kind: keyUnknown}, 3
	}
	switch buf[2] {
	case '1', '7':
		return KeyEvent{Kind: keyHome}, 4
	case '4', '8':
		return KeyEvent{Kind: keyEnd}, 4
	case '3':
		return KeyEvent{Kind: keyDelete}, 4
	case '5':
		return KeyEvent{Kind: keyPgUp}, 4
	case '6':
		return KeyEvent{Kind: keyPgDn}, 4
	}
	return KeyEvent{Kind: keyUnknown}, 4
}

// escDelay disambiguates a lone ESC from the start of a sequence (matches
// curses set_escdelay(25); VTIME is too coarse on Linux).
const escDelay = 25 * time.Millisecond

// keyReader turns a byte stream into KeyEvents on a buffered channel.
// waitMore reports whether more input arrives within d; in production it is
// fdWaitMore(0) (a select on stdin), in tests a scripted hook.
type keyReader struct {
	src      io.Reader
	waitMore func(time.Duration) bool
	events   chan KeyEvent
}

func newKeyReader(src io.Reader, waitMore func(time.Duration) bool) *keyReader {
	return &keyReader{src: src, waitMore: waitMore, events: make(chan KeyEvent, 64)}
}

// start runs the reader in its own goroutine.
func (r *keyReader) start() {
	go r.run()
}

// run reads until EOF/error, pushing events, then closes the channel.
func (r *keyReader) run() {
	defer close(r.events)
	var pending []byte
	buf := make([]byte, 64)
	for {
		n, err := r.src.Read(buf)
		if n > 0 {
			pending = r.drain(append(pending, buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// drain parses complete events out of pending, returning the unparsed rest.
// On an incomplete sequence it waits escDelay for more bytes; on timeout a
// lone ESC becomes keyEscape, anything longer is dropped as keyUnknown.
func (r *keyReader) drain(pending []byte) []byte {
	for len(pending) > 0 {
		key, consumed := parseKey(pending)
		if key.Kind != keyNone {
			r.events <- key
			pending = pending[consumed:]
			continue
		}
		if r.waitMore != nil && r.waitMore(escDelay) {
			return pending // more bytes coming; the next Read completes it
		}
		if len(pending) == 1 && pending[0] == 0x1b {
			r.events <- KeyEvent{Kind: keyEscape}
		} else {
			r.events <- KeyEvent{Kind: keyUnknown}
		}
		return nil
	}
	return pending
}

// fdWaitMore returns a waitMore hook polling fd with select(2).
func fdWaitMore(fd int) func(time.Duration) bool {
	return func(d time.Duration) bool {
		return pollReadable(fd, d)
	}
}

// pollReadable reports whether fd becomes readable within d. Uses raw
// syscalls because syscall.Select has different signatures on darwin and
// linux, and the numbers are declared locally because syscall.SYS_SELECT is
// undefined on linux/arm64 (no select syscall there) and SYS_PSELECT6 is
// undefined on darwin.
func pollReadable(fd int, d time.Duration) bool {
	var set syscall.FdSet
	fdSet(&set, fd)
	if runtime.GOOS == "darwin" {
		tv := syscall.NsecToTimeval(d.Nanoseconds())
		const sysSelect = 93 // SYS_SELECT on darwin (both arches)
		n, _, errno := syscall.Syscall6(sysSelect, uintptr(fd+1),
			uintptr(unsafe.Pointer(&set)), 0, 0, uintptr(unsafe.Pointer(&tv)), 0)
		return errno == 0 && n > 0
	}
	// linux: select(2) does not exist on arm64; use pselect6 (270 on amd64,
	// 72 on arm64) with a *Timespec timeout and a nil sigmask.
	sysno := 270
	if runtime.GOARCH == "arm64" {
		sysno = 72
	}
	ts := syscall.NsecToTimespec(d.Nanoseconds())
	n, _, errno := syscall.Syscall6(uintptr(sysno), uintptr(fd+1),
		uintptr(unsafe.Pointer(&set)), 0, 0, uintptr(unsafe.Pointer(&ts)), 0)
	return errno == 0 && n > 0
}

// fdSet sets fd in an FdSet (Bits is [32]int32 on darwin, [16]int64 on
// linux).
func fdSet(set *syscall.FdSet, fd int) {
	idx, shift := fd/32, uint(fd%32)
	if runtime.GOOS != "darwin" {
		idx, shift = fd/64, uint(fd%64)
	}
	set.Bits[idx] |= 1 << shift
}

// eventMux multiplexes the inputs a TUI loop selects on: keys (from the
// reader goroutine) and resize notifications (SIGWINCH → re-poll Term.Size).
// Callers add their own timer channels in the same select (this replaces
// scr.timeout(90) picker debounce). SIGINT is injected as a Ctrl-C key;
// SIGTERM runs cleanup (terminal restore) and exits.
type eventMux struct {
	Keys   chan KeyEvent
	Resize chan struct{}
	sigch  chan os.Signal
}

func newEventMux(kr *keyReader, cleanup func()) *eventMux {
	m := &eventMux{
		Keys:   kr.events,
		Resize: make(chan struct{}, 1),
		sigch:  make(chan os.Signal, 4),
	}
	signal.Notify(m.sigch, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)
	go m.dispatch(cleanup)
	return m
}

func (m *eventMux) dispatch(cleanup func()) {
	for sig := range m.sigch {
		switch sig {
		case syscall.SIGWINCH:
			select {
			case m.Resize <- struct{}{}:
			default:
			}
		case syscall.SIGINT:
			m.Keys <- KeyEvent{Kind: keyCtrlC}
		case syscall.SIGTERM:
			if cleanup != nil {
				cleanup()
			}
			os.Exit(1)
		}
	}
}

// stop detaches from signal delivery.
func (m *eventMux) stop() {
	signal.Stop(m.sigch)
	close(m.sigch)
}

// ---- TUI: interactive App (Python's App class) ----
//
// The App is deliberately separable from real tty I/O: it reads KeyEvents
// from a channel, watches a resize channel, asks a size func for dimensions,
// and renders into frameBuffers passed to a flush func. Production wires
// those to eventMux/Term (runTUI); tests wire them to fakes and drive the
// app synchronously.

// runeLen is Python's len() for our purposes (all TUI glyphs are width-1).
func runeLen(s string) int { return utf8.RuneCountInString(s) }

// ljust pads s with spaces to width w runes (Python str.ljust).
func ljust(s string, w int) string {
	if n := runeLen(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// trimLastRune drops the last rune (Python buf[:-1]; buffers here only ever
// hold ASCII, but keep it rune-safe anyway).
func trimLastRune(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	return string(r[:len(r)-1])
}

// firstErr is Python's `errs[0] if errs else "?"`.
func firstErr(errs []string) string {
	if len(errs) > 0 {
		return errs[0]
	}
	return "?"
}

// kindFor maps an ok flag to a status kind ("ok" if ok else "error").
func kindFor(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}

// orEmpty is Python's `value or '(empty)'`.
func orEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// filterItems is the picker's type-to-filter: case-insensitive substring.
func filterItems(items []string, query string) []string {
	q := strings.ToLower(query)
	var out []string
	for _, it := range items {
		if strings.Contains(strings.ToLower(it), q) {
			out = append(out, it)
		}
	}
	return out
}

// wrapText wraps each paragraph of text to width runes, preserving leading
// indentation (Python App._wrap).
func wrapText(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		if strings.TrimSpace(para) == "" {
			out = append(out, "")
			continue
		}
		indent := len(para) - len(strings.TrimLeft(para, " \t\f\v\r"))
		pad := strings.Repeat(" ", indent)
		cur := pad
		for _, wd := range strings.Fields(para) {
			switch {
			case runeLen(cur)+runeLen(wd)+1 > width && strings.TrimSpace(cur) != "":
				out = append(out, cur)
				cur = pad + wd
			case strings.TrimSpace(cur) != "":
				cur += " " + wd
			default:
				cur += wd
			}
		}
		out = append(out, cur)
	}
	return out
}

// resizeKey is the synthetic event delivered on terminal resize or a picker
// timeout; like curses' KEY_RESIZE (or getch's -1) it means "loop around and
// redraw" — every handler simply ignores it.
var resizeKey = KeyEvent{Kind: keyNone}

// App is the interactive configurator (Python's App).
type App struct {
	sess     *Session
	icons    bool
	color256 bool

	status        string
	statusKind    string
	focus         string // "cats" or "opts"
	catIdx        int
	optIdx        int
	optScroll     int
	docScroll     int
	search        string
	searchMode    bool
	searchResults []string

	byCat      map[string][]string
	categories []string

	mu              sync.Mutex
	updInfo         *updateInfo
	updateAnnounced bool

	rows int
	cols int
	fb   *frameBuffer // last frame drawn (tests inspect it)

	keys    chan KeyEvent
	resize  chan struct{}
	sizeFn  func() (int, int)
	flushFn func(*frameBuffer)
}

// newApp builds the App state (Python App.__init__ minus the curses bits):
// categories from the schema plus the synthetic Utils category, and the
// background update check. I/O plumbing (keys/resize/sizeFn/flushFn) is
// attached by the caller; rows/cols default to 80x24 until sized.
func newApp(sess *Session, icons, color256 bool) *App {
	a := &App{
		sess:       sess,
		icons:      icons,
		color256:   color256,
		statusKind: "info",
		focus:      "cats",
		byCat:      map[string][]string{},
		rows:       24,
		cols:       80,
	}
	for _, c := range categoryOrder {
		var names []string
		for n, o := range sess.Schema {
			if o.Category == c && platformVisible(o) {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			a.byCat[c] = names
		}
	}
	for _, c := range categoryOrder {
		if _, ok := a.byCat[c]; ok {
			a.categories = append(a.categories, c)
		}
	}
	a.categories = append(a.categories, utilsCategory)
	a.startUpdateCheck()
	return a
}

// startUpdateCheck mirrors App._start_update_check: one background fetch,
// stored under the mutex for the header badge / U key / help text.
func (a *App) startUpdateCheck() {
	if updateCheckDisabled() {
		return
	}
	go func() {
		info := checkForUpdate(false, nowSeconds())
		a.mu.Lock()
		a.updInfo = info
		a.mu.Unlock()
	}()
}

func (a *App) updateInfo() *updateInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updInfo
}

// nextEvent waits for the next key, a resize, or (when timeout > 0) a
// timeout — the select replaces curses' blocking getch and scr.timeout(90).
// A closed keys channel means stdin went away; treat it as Ctrl-C (Python's
// KeyboardInterrupt path). Resize re-polls the size and reports resizeKey.
func (a *App) nextEvent(timeout time.Duration) KeyEvent {
	var tick <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		tick = timer.C
	}
	select {
	case k, ok := <-a.keys:
		if !ok {
			return KeyEvent{Kind: keyCtrlC}
		}
		return k
	case <-a.resize:
		if a.sizeFn != nil {
			a.rows, a.cols = a.sizeFn()
		}
		return resizeKey
	case <-tick:
		return resizeKey
	}
}

// flush pushes the current frame (production: one write to the tty).
func (a *App) flush() {
	if a.flushFn != nil {
		a.flushFn(a.fb)
	}
}

// msg sets the footer status line (Python App._msg).
func (a *App) msg(text, kind string) {
	a.status = text
	a.statusKind = kind
}

// newScreen starts a fresh full-screen frame; the leading ESC[2J is Python's
// scr.erase() (our frames are full redraws).
func (a *App) newScreen() {
	a.fb = newFrameBuffer(a.rows, a.cols)
	//nolint:errcheck // bytes.Buffer writes cannot fail
	a.fb.buf.Write([]byte("\x1b[2J"))
}

// run is Python App.run: draw, wait, dispatch, until a handler quits.
func (a *App) run() {
	for {
		a.draw()
		k := a.nextEvent(0)
		if k == resizeKey {
			continue
		}
		if !a.handleKey(k) {
			break
		}
	}
}

// draw renders the main three-pane screen (Python App.draw).
func (a *App) draw() {
	if info := a.updateInfo(); info != nil && info.Outdated && !a.updateAnnounced {
		a.updateAnnounced = true
		if a.status == "" {
			a.msg(fmt.Sprintf("SpookiUI %s is available — press U to update", info.Latest), "warn")
		}
	}
	a.newScreen()
	a.drawHeader(a.cols)
	a.drawColumns(1, a.rows-3, a.cols)
	a.drawFooter(a.rows, a.cols)
	a.flush()
}

func (a *App) drawHeader(w int) {
	title := " SpookiUI · live Ghostty configurator "
	var flags []string
	if info := a.updateInfo(); info != nil && info.Outdated {
		flags = append(flags, "⬆ UPDATE "+info.Latest)
	}
	if a.sess.AutoApply {
		flags = append(flags, "AUTO-APPLY:ON")
	} else {
		flags = append(flags, "AUTO-APPLY:OFF")
	}
	if a.sess.Dirty {
		flags = append(flags, "UNSAVED*")
	}
	if a.sess.AutoApply && canReload {
		flags = append(flags, "live")
	} else {
		flags = append(flags, "manual")
	}
	right := " " + strings.Join(flags, " · ") + " "
	bar := title + strings.Repeat(" ", max(0, w-runeLen(title)-runeLen(right))) + right
	a.fb.addstr(0, 0, bar, textAttr{pair: 1, bold: true})
}

func (a *App) drawColumns(top, bottom, w int) {
	catW := 22
	if a.icons {
		catW = 24
	}
	optW := max(28, min(40, (w-catW)/2))
	detX := catW + optW + 1

	for i, cat := range a.categories {
		y := top + i
		if y > bottom {
			break
		}
		var attr textAttr
		label := " " + cat
		if a.icons {
			icon, ok := categoryIcons[cat]
			if !ok {
				icon = defaultCategoryIcon
			}
			name := cat
			if cat == utilsCategory {
				name = "Utils"
			}
			label = " " + icon + "  " + name
		}
		switch {
		case a.searchMode:
			attr = textAttr{pair: 4}
		case i == a.catIdx:
			if a.focus == "cats" {
				attr = textAttr{pair: 9, bold: true}
			} else {
				attr = textAttr{pair: 5, bold: true}
			}
		}
		a.fb.addstr(y, 0, clipRunes(ljust(label, catW), catW), attr)
	}
	for y := top; y <= bottom; y++ {
		a.fb.addstr(y, catW, "│", textAttr{pair: 4})
		a.fb.addstr(y, catW+optW, "│", textAttr{pair: 4})
	}

	if a.curCat() == utilsCategory {
		a.drawUtilsMenu(top, bottom, catW, optW)
		a.drawUtilsDetail(top, bottom, detX, w-detX-1)
		return
	}

	names := a.currentNames()
	rows := bottom - top + 1
	if a.optIdx < a.optScroll {
		a.optScroll = a.optIdx
	} else if a.optIdx >= a.optScroll+rows {
		a.optScroll = a.optIdx - rows + 1
	}
	for r := 0; r < rows; r++ {
		i := a.optScroll + r
		if i >= len(names) {
			break
		}
		name := names[i]
		y := top + r
		selected := i == a.optIdx && (a.focus == "opts" || a.searchMode)
		over := a.sess.IsOverridden(name)
		mark := " "
		if over {
			mark = "●"
		}
		x0 := catW + 1
		var base textAttr
		if selected {
			base = textAttr{pair: 3, bold: true}
		}
		markAttr := textAttr{pair: 4, bold: selected}
		if over {
			markAttr.pair = 6
		}
		a.fb.addstr(y, x0, mark, markAttr)
		nm := clipRunes(name, optW-4)
		a.fb.addstr(y, x0+2, clipRunes(ljust(nm, optW-3), optW-3), base)
	}
	if len(names) > rows {
		a.fb.addstr(top, catW+optW-1, "↕", textAttr{pair: 5})
	}

	a.drawDetail(top, bottom, detX, w-detX-1)
}

func (a *App) drawDetail(top, bottom, x, width int) {
	if width < 10 {
		return
	}
	opt := a.currentOption()
	if opt == nil {
		a.fb.addstr(top, x, "no options", textAttr{pair: 4})
		return
	}
	y := top
	a.fb.addstr(y, x, opt.Name, textAttr{pair: 10, bold: true})
	y++
	kind := opt.Kind
	if opt.IsList && !containsString([]string{"list", "font", "palette", "keybind"}, opt.Kind) {
		kind += " · list"
	}
	a.fb.addstr(y, x, "type: "+kind, textAttr{pair: 2})
	y++

	if opt.IsList {
		vals := a.sess.EffectiveList(opt.Name)
		a.fb.addstr(y, x, "value:", textAttr{pair: 4})
		y++
		for i, v := range vals {
			if i >= 6 {
				break
			}
			a.drawValueLine(y, x+2, width-2, opt, v, false)
			y++
		}
		if len(vals) > 6 {
			a.fb.addstr(y, x+2, fmt.Sprintf("… +%d more", len(vals)-6), textAttr{pair: 4})
			y++
		}
		if len(vals) == 0 {
			a.fb.addstr(y, x+2, "(none)", textAttr{pair: 4})
			y++
		}
	} else {
		cur := a.sess.Effective(opt.Name)
		a.fb.addstr(y, x, "value: ", textAttr{pair: 4})
		a.drawValueLine(y, x+7, width-7, opt, cur, true)
		y++
		a.fb.addstr(y, x, clipRunes("default: "+orEmpty(opt.Default), width), textAttr{pair: 4})
		y++
	}

	over := a.sess.IsOverridden(opt.Name)
	overStr, overAttr := "no (default)", textAttr{pair: 4}
	if over {
		overStr, overAttr = "yes", textAttr{pair: 6}
	}
	a.fb.addstr(y, x, "changed: "+overStr, overAttr)
	y++
	if opt.ReloadNote != "" {
		a.fb.addstr(y, x, "⚠ "+opt.ReloadNote, textAttr{pair: 8})
		y++
	}
	if len(opt.Values) > 0 && (opt.Kind == "enum" || opt.Kind == "bool") {
		a.fb.addstr(y, x, "choices: "+strings.Join(opt.Values, ", "), textAttr{pair: 5})
		y++
	}
	y++
	if (opt.Name == "theme" || opt.Kind == "color" || opt.Kind == "theme" ||
		opt.Kind == "palette" || opt.Category == "Colors & Theme") && y+3 <= bottom {
		a.fb.addstr(y, x, "─ preview "+strings.Repeat("─", max(0, width-10)), textAttr{pair: 4})
		y++
		if used := a.drawColorPreview(y, x, width, a.effectiveColors("", false)); used > 0 {
			y += used + 1
		}
	}
	a.fb.addstr(y, x, "─ docs "+strings.Repeat("─", max(0, width-7)), textAttr{pair: 4})
	y++
	doc := opt.Doc
	if doc == "" {
		doc = "(no documentation)"
	}
	docLines := wrapText(doc, width)
	for _, line := range docLines[min(a.docScroll, len(docLines)):] {
		if y > bottom {
			a.fb.addstr(bottom, x, "… (scroll docs: [ ])", textAttr{pair: 4})
			break
		}
		a.fb.addstr(y, x, line, textAttr{pair: 4})
		y++
	}
}

// drawValueLine draws a value, prefixed with a ██ colour swatch when it
// parses as a hex colour (Python App._draw_value_line).
func (a *App) drawValueLine(y, x, width int, opt *Option, value string, bold bool) {
	v := value
	if opt.IsList {
		if i := strings.LastIndex(v, "="); i >= 0 {
			v = v[i+1:]
		}
	}
	if _, _, _, ok := parseHex(v); ok {
		sw := textAttr{pair: 5}
		if idx, ok2 := hexTo256(v); a.color256 && ok2 {
			sw = textAttr{fg256: idx}
		}
		a.fb.addstr(y, x, "██ ", sw)
		a.fb.addstr(y, x+3, clipRunes(value, width-3), textAttr{pair: 5, bold: bold})
		return
	}
	a.fb.addstr(y, x, clipRunes(value, width), textAttr{pair: 5, bold: bold})
}

// effColors is App._effective_colors' result.
type effColors struct {
	palette []string // 16 entries, "#000000" where unset
	fg      string
	bg      string
}

// effectiveColors resolves the colours that would actually render: schema
// defaults, then the active (or overridden) theme, then explicit config
// overrides. useOverride previews a theme without touching config.
func (a *App) effectiveColors(themeOverride string, useOverride bool) *effColors {
	pal := map[int]string{}
	if popt := a.sess.Schema["palette"]; popt != nil {
		for _, d := range popt.Defaults {
			idx, col, _ := strings.Cut(d, "=")
			if i, err := strconv.Atoi(strings.TrimSpace(idx)); err == nil {
				pal[i] = strings.TrimSpace(col)
			}
		}
	}
	fg, bg := "#ffffff", "#000000"
	if o, ok := a.sess.Schema["foreground"]; ok {
		fg = o.Default
	}
	if o, ok := a.sess.Schema["background"]; ok {
		bg = o.Default
	}

	themeVal := a.sess.Effective("theme")
	if useOverride {
		themeVal = themeOverride
	}
	if themeVal != "" {
		if tc := parseThemeColors(themeVariantName(themeVal)); tc != nil {
			for i, c := range tc.Palette {
				pal[i] = c
			}
			if tc.Foreground != "" {
				fg = tc.Foreground
			}
			if tc.Background != "" {
				bg = tc.Background
			}
		}
	}

	if !useOverride {
		if fo, ok := a.sess.Cfg.GetValue("foreground"); ok && fo != "" {
			fg = fo
		}
		if bo, ok := a.sess.Cfg.GetValue("background"); ok && bo != "" {
			bg = bo
		}
		for _, v := range a.sess.Cfg.GetValues("palette") {
			idx, col, _ := strings.Cut(v, "=")
			if i, err := strconv.Atoi(strings.TrimSpace(idx)); err == nil {
				pal[i] = strings.TrimSpace(col)
			}
		}
	}

	palette := make([]string, 16)
	for i := range palette {
		if c, ok := pal[i]; ok {
			palette[i] = c
		} else {
			palette[i] = "#000000"
		}
	}
	return &effColors{palette: palette, fg: fg, bg: bg}
}

// drawColorPreview renders the compact theme card (two swatch rows + a
// sample line). Returns rows drawn (0 when colours are unavailable).
func (a *App) drawColorPreview(y, x, width int, colors *effColors) int {
	if !a.color256 || width < 18 {
		return 0
	}
	rows := 0
	for band := 0; band < 2; band++ {
		xx := x
		for i := 0; i < 8; i++ {
			attr := textAttr{pair: 4}
			if idx, ok := hexTo256(colors.palette[band*8+i]); ok {
				attr = textAttr{fg256: idx}
			}
			a.fb.addstr(y+band, xx, "██", attr)
			xx += 3
		}
		rows++
	}
	attr := textAttr{pair: 4, bold: true}
	fi, fok := hexTo256(colors.fg)
	bi, bok := hexTo256(colors.bg)
	if fok || bok {
		attr = textAttr{bold: true}
		if fok {
			attr.fg256 = fi
		}
		if bok {
			attr.bg256 = bi
		}
	}
	a.fb.addstr(y+2, x, clipRunes(" AaBbCc 123 #!$ ", width), attr)
	rows++
	return rows
}

// curCat is Python App.cur_cat ("" when in search mode or no categories).
func (a *App) curCat() string {
	if a.searchMode || len(a.categories) == 0 {
		return ""
	}
	return a.categories[a.catIdx]
}

// currentNames is Python App.current_names (nil for the Utils category).
func (a *App) currentNames() []string {
	if a.searchMode {
		return a.searchResults
	}
	if len(a.categories) == 0 {
		return nil
	}
	cat := a.categories[a.catIdx]
	if cat == utilsCategory {
		return nil
	}
	return a.byCat[cat]
}

// currentOption is Python App.current_option (clamps optIdx as a side effect).
func (a *App) currentOption() *Option {
	names := a.currentNames()
	if len(names) == 0 {
		return nil
	}
	a.optIdx = max(0, min(a.optIdx, len(names)-1))
	return a.sess.Schema[names[a.optIdx]]
}

func (a *App) drawFooter(h, w int) {
	kindmap := map[string]int{"ok": 6, "error": 7, "warn": 8, "info": 2}
	pair, ok := kindmap[a.statusKind]
	if !ok {
		pair = 2
	}
	a.fb.addstr(h-2, 0, clipRunes(a.status, w), textAttr{pair: pair, bold: true})
	var hints string
	switch {
	case a.searchMode:
		hints = " type to filter · ↑↓ move · Enter edit · Esc exit search "
	case a.focus == "cats":
		hints = " ↑↓ category · →/Enter options · / search · a auto-apply · v utils · d changes · ? help · q quit "
	default:
		hints = " ↑↓ option · Enter/→ edit · ← back · u reset · s save · r reload · / search · ? help · q quit "
	}
	a.fb.addstr(h-1, 0, ljust(hints, w), textAttr{pair: 1})
}

// handleKey is Python App.handle_key. Returns false to quit the run loop.
func (a *App) handleKey(k KeyEvent) bool {
	if a.searchMode {
		return a.handleSearchKey(k)
	}

	if k.Kind == keyCtrlC || k.Kind == keyCtrlX ||
		(k.Kind == keyRune && (k.Rune == 'q' || k.Rune == 'Q')) {
		return a.quit()
	}
	if k.Kind == keyRune {
		switch k.Rune {
		case '?':
			a.help()
			return true
		case '/':
			a.enterSearch()
			return true
		case 'a', 'A':
			a.sess.AutoApply = !a.sess.AutoApply
			if a.sess.AutoApply {
				a.msg("auto-apply ON — changes go live", "info")
			} else {
				a.msg("auto-apply OFF — staged only", "info")
			}
			return true
		case 'd', 'D':
			a.changesOverlay()
			return true
		case 's', 'S':
			ok, m := a.sess.Apply()
			a.msg(m, kindFor(ok))
			return true
		case 'r':
			rok, m := reloadGhostty()
			if rok {
				a.msg("reload: "+m, "ok")
			} else {
				a.msg("reload: "+m, "warn")
			}
			return true
		case 'R':
			if a.confirm("Revert ALL changes to session start?") {
				_, m := a.sess.RevertAll()
				a.msg(m, "ok")
			}
			return true
		case 'X':
			if a.confirm("Wipe config & restore ALL Ghostty defaults? (backup kept)") {
				ok, m := a.sess.RestoreDefaults()
				a.msg(m, kindFor(ok))
			}
			return true
		case 'U':
			a.doSelfUpdate()
			return true
		case 'p', 'P':
			a.profilesOverlay()
			return true
		case 'c', 'C':
			a.doctorOverlay()
			return true
		case 'v', 'V':
			a.utilsOverlay()
			return true
		case '[':
			a.docScroll = max(0, a.docScroll-1)
			return true
		case ']':
			a.docScroll++
			return true
		}
	}
	if k.Kind == keyTab {
		if a.focus == "cats" {
			a.focus = "opts"
		} else {
			a.focus = "cats"
		}
		return true
	}

	if a.focus == "cats" {
		return a.handleCatKey(k)
	}
	return a.handleOptKey(k)
}

func (a *App) handleCatKey(k KeyEvent) bool {
	n := len(a.categories)
	switch {
	case k.Kind == keyUp || (k.Kind == keyRune && k.Rune == 'k'):
		a.catIdx = (a.catIdx - 1 + n) % n
		a.optIdx, a.optScroll, a.docScroll = 0, 0, 0
	case k.Kind == keyDown || (k.Kind == keyRune && k.Rune == 'j'):
		a.catIdx = (a.catIdx + 1) % n
		a.optIdx, a.optScroll, a.docScroll = 0, 0, 0
	case k.Kind == keyRight || k.Kind == keyEnter || (k.Kind == keyRune && k.Rune == 'l'):
		if a.curCat() == utilsCategory {
			a.utilsOverlay()
		} else {
			a.focus = "opts"
		}
	}
	return true
}

func (a *App) handleOptKey(k KeyEvent) bool {
	names := a.currentNames()
	switch {
	case k.Kind == keyUp || (k.Kind == keyRune && k.Rune == 'k'):
		a.optIdx = (a.optIdx - 1 + max(1, len(names))) % max(1, len(names))
		a.docScroll = 0
	case k.Kind == keyDown || (k.Kind == keyRune && k.Rune == 'j'):
		a.optIdx = (a.optIdx + 1) % max(1, len(names))
		a.docScroll = 0
	case k.Kind == keyPgDn:
		a.optIdx = min(len(names)-1, a.optIdx+10)
		a.docScroll = 0
	case k.Kind == keyPgUp:
		a.optIdx = max(0, a.optIdx-10)
		a.docScroll = 0
	case k.Kind == keyLeft || (k.Kind == keyRune && k.Rune == 'h'):
		a.focus = "cats"
	case k.Kind == keyRune && k.Rune == 'u':
		a.resetCurrent()
	case k.Kind == keyEnter || k.Kind == keyRight || (k.Kind == keyRune && k.Rune == 'l'):
		if a.curCat() == utilsCategory {
			a.utilsOverlay()
		} else {
			a.editCurrent()
		}
	}
	return true
}

// ---- Search ----

func (a *App) enterSearch() {
	a.search = ""
	a.searchMode = true
	var names []string
	for n, o := range a.sess.Schema {
		if platformVisible(o) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	a.searchResults = names
	a.optIdx, a.optScroll = 0, 0
}

func (a *App) handleSearchKey(k KeyEvent) bool {
	switch k.Kind {
	case keyEscape:
		a.searchMode = false
		a.focus = "opts"
		a.optIdx, a.optScroll = 0, 0
		return true
	case keyUp:
		a.optIdx = max(0, a.optIdx-1)
		a.docScroll = 0
		return true
	case keyDown:
		a.optIdx = min(len(a.searchResults)-1, a.optIdx+1)
		a.docScroll = 0
		return true
	case keyPgDn:
		a.optIdx = min(len(a.searchResults)-1, a.optIdx+10)
		return true
	case keyPgUp:
		a.optIdx = max(0, a.optIdx-10)
		return true
	case keyEnter:
		if len(a.searchResults) > 0 {
			a.editCurrent()
		}
		return true
	case keyBackspace:
		a.search = trimLastRune(a.search)
	case keyRune: // exactly 32–126, Python's `32 <= ch < 127`
		a.search += string(k.Rune)
	default: // keyUTF8 and friends are ignored
		return true
	}
	q := strings.ToLower(a.search)
	var res []string
	for n, o := range a.sess.Schema {
		if platformVisible(o) &&
			(strings.Contains(strings.ToLower(n), q) || strings.Contains(strings.ToLower(o.Doc), q)) {
			res = append(res, n)
		}
	}
	sort.Strings(res)
	a.searchResults = res
	a.optIdx, a.optScroll = 0, 0
	a.msg(fmt.Sprintf("search: %s   (%d matches)", a.search, len(res)), "info")
	return true
}

// ---- Editing ----

// editCurrent dispatches on the option kind (Python App.edit_current).
func (a *App) editCurrent() {
	opt := a.currentOption()
	if opt == nil {
		return
	}
	switch opt.Kind {
	case "bool":
		a.editBool(opt)
	case "enum":
		a.editEnum(opt)
	case "theme":
		a.editTheme(opt)
	case "font":
		a.editFont(opt)
	case "int", "float":
		if lo, hi, step, ok := sliderRange(opt); ok {
			a.editSlider(opt, lo, hi, step)
		} else {
			a.editNumber(opt)
		}
	case "list", "keybind", "palette":
		a.editList(opt)
	default:
		a.editText(opt)
	}
}

// commitScalar stages a scalar and applies it live when auto-apply is on
// (Python App._commit_scalar; its unused `preview` arg is dropped).
func (a *App) commitScalar(opt *Option, value string) (bool, []string) {
	snap := append([]string(nil), a.sess.Cfg.Lines...)
	a.sess.Cfg.SetScalar(opt.Name, value)
	if !a.sess.AutoApply {
		a.sess.Dirty = true
		return true, nil
	}
	text := a.sess.Cfg.Render()
	ok, errs := validateConfig(text)
	if !ok {
		a.sess.Cfg.Lines = snap
		return false, errs
	}
	a.sess.ensureBackup()
	if err := a.sess.Cfg.WriteText(text); err != nil {
		a.sess.Cfg.Lines = snap
		return false, []string{err.Error()}
	}
	a.sess.Dirty = false
	reloadGhostty()
	return true, nil
}

// commitList is _commit_scalar for list options (Python App._commit_list).
func (a *App) commitList(opt *Option, values []string) (bool, []string) {
	snap := append([]string(nil), a.sess.Cfg.Lines...)
	a.sess.Cfg.SetList(opt.Name, values)
	if !a.sess.AutoApply {
		a.sess.Dirty = true
		return true, nil
	}
	text := a.sess.Cfg.Render()
	ok, errs := validateConfig(text)
	if !ok {
		a.sess.Cfg.Lines = snap
		return false, errs
	}
	a.sess.ensureBackup()
	if err := a.sess.Cfg.WriteText(text); err != nil {
		a.sess.Cfg.Lines = snap
		return false, []string{err.Error()}
	}
	a.sess.Dirty = false
	reloadGhostty()
	return true, nil
}

func (a *App) resetCurrent() {
	opt := a.currentOption()
	if opt == nil || !a.sess.IsOverridden(opt.Name) {
		a.msg("already at default", "info")
		return
	}
	snap := append([]string(nil), a.sess.Cfg.Lines...)
	a.sess.Cfg.Unset(opt.Name)
	if a.sess.AutoApply {
		text := a.sess.Cfg.Render()
		ok, errs := validateConfig(text)
		if !ok {
			a.sess.Cfg.Lines = snap
			a.msg("reset failed: "+firstErr(errs), "error")
			return
		}
		a.sess.ensureBackup()
		if err := a.sess.Cfg.WriteText(text); err != nil {
			a.sess.Cfg.Lines = snap
			a.msg("reset failed: "+err.Error(), "error")
			return
		}
		reloadGhostty()
	}
	a.sess.Dirty = !a.sess.AutoApply
	a.msg(fmt.Sprintf("%s reset to default (%s)", opt.Name, orEmpty(opt.Default)), "ok")
}

func (a *App) doSelfUpdate() {
	info := a.updateInfo()
	if info == nil || !info.Outdated {
		a.msg(fmt.Sprintf("SpookiUI v%s is already up to date", version), "info")
		return
	}
	if !a.confirm(fmt.Sprintf("Update SpookiUI to %s? (a backup is kept)", info.Latest)) {
		return
	}
	a.msg(fmt.Sprintf("updating to %s…", info.Latest), "info")
	a.draw()
	ok, m := selfUpdate(nowSeconds())
	a.msg(m, kindFor(ok))
}

func (a *App) editBool(opt *Option) {
	cur := a.sess.Effective(opt.Name)
	newVal := "true"
	if cur == "true" {
		newVal = "false"
	}
	ok, errs := a.commitScalar(opt, newVal)
	if ok {
		a.msg(fmt.Sprintf("%s = %s%s", opt.Name, newVal, a.liveTag()), "ok")
	} else {
		a.msg("invalid: "+firstErr(errs), "error")
	}
}

// liveTag is Python's `" (live)" if auto_apply else " (staged)"` suffix.
func (a *App) liveTag() string {
	if a.sess.AutoApply {
		return " (live)"
	}
	return " (staged)"
}

// snap captures the config lines for later rollback (Python App._snap).
func (a *App) snap() []string {
	return append([]string(nil), a.sess.Cfg.Lines...)
}

// restore undoes any (previewed) edits back to snap, re-applying live
// (Python App._restore).
func (a *App) restore(snap []string) {
	a.sess.Cfg.Lines = snap
	if a.sess.AutoApply {
		//nolint:errcheck // best-effort rollback write, like the Python
		_ = a.sess.Cfg.Write()
		reloadGhostty()
	}
	a.sess.Dirty = strings.TrimRight(a.sess.Cfg.Render(), "\n") !=
		strings.TrimRight(a.sess.OriginalText, "\n")
}

// report is Python App._report: validate-and-report after a commit.
func (a *App) report(opt *Option, value string, ok bool, errs []string) {
	if ok {
		a.msg(fmt.Sprintf("%s = %s%s", opt.Name, value, a.liveTag()), "ok")
	} else {
		a.msg("invalid: "+firstErr(errs), "error")
	}
}

func (a *App) editEnum(opt *Option) {
	snap := a.snap()
	cur := a.sess.Effective(opt.Name)
	choice, ok := a.picker(opt.Name, opt.Values, cur,
		func(v string) { _, _ = a.commitScalar(opt, v) }, nil)
	if !ok {
		a.restore(snap)
		a.msg("cancelled", "info")
		return
	}
	ok2, errs := a.commitScalar(opt, choice)
	a.report(opt, choice, ok2, errs)
}

func (a *App) editTheme(opt *Option) {
	a.msg("loading themes…", "info")
	a.draw()
	themes := listThemes()
	if len(themes) == 0 {
		a.msg("no themes found", "warn")
		return
	}
	snap := a.snap()
	cur := a.sess.Effective(opt.Name)
	choice, ok := a.picker("theme", themes, cur,
		func(v string) { _, _ = a.commitScalar(opt, v) },
		func(item string, x, y, width int) { a.drawThemeCard(item, x, y, width) })
	if !ok {
		a.restore(snap)
		a.msg("cancelled", "info")
		return
	}
	ok2, errs := a.commitScalar(opt, choice)
	a.report(opt, choice, ok2, errs)
}

func (a *App) editFont(opt *Option) {
	a.msg("loading fonts…", "info")
	a.draw()
	fonts := listFonts()
	if len(fonts) == 0 {
		a.editText(opt)
		return
	}
	snap := a.snap()
	curlist := a.sess.EffectiveList(opt.Name)
	cur := ""
	if len(curlist) > 0 {
		cur = curlist[0]
	}
	choice, ok := a.picker(opt.Name+" (primary)", fonts, cur,
		func(v string) { _, _ = a.commitList(opt, []string{v}) }, nil)
	if !ok {
		a.restore(snap)
		a.msg("cancelled", "info")
		return
	}
	ok2, errs := a.commitList(opt, []string{choice})
	a.report(opt, choice, ok2, errs)
}

func (a *App) editNumber(opt *Option) {
	snap := a.snap()
	cur := a.sess.Effective(opt.Name)
	isFloat := opt.Kind == "float"
	step := 1.0
	if isFloat {
		step = 0.05
	}
	val := 0.0
	if cur != "" {
		if f, err := strconv.ParseFloat(cur, 64); err == nil {
			val = f
		}
	}
	buf := cur
	for {
		a.promptBar(opt.Name+" = ", buf, "↑↓ / +- step · type value · Enter apply · Esc cancel")
		k := a.nextEvent(0)
		switch {
		case k.Kind == keyEscape:
			a.restore(snap)
			a.msg("cancelled", "info")
			return
		case k.Kind == keyEnter:
			v := strings.TrimSpace(buf)
			ok, errs := a.commitScalar(opt, v)
			if !ok {
				a.restore(snap)
			}
			a.report(opt, v, ok, errs)
			return
		case k.Kind == keyUp || (k.Kind == keyRune && (k.Rune == '+' || k.Rune == '=')):
			val = numOr(buf, val) + step
			buf = fmtNum(val, isFloat)
			_, _ = a.commitScalar(opt, buf)
		case k.Kind == keyDown || (k.Kind == keyRune && (k.Rune == '-' || k.Rune == '_')):
			val = numOr(buf, val) - step
			buf = fmtNum(val, isFloat)
			_, _ = a.commitScalar(opt, buf)
		case k.Kind == keyBackspace:
			buf = trimLastRune(buf)
		case k.Kind == keyRune && strings.ContainsRune("0123456789.-", k.Rune):
			buf += string(k.Rune)
		}
	}
}

// numOr is Python App._num: parse buf, else keep the running value.
func numOr(buf string, fallback float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(buf), 64); err == nil {
		return f
	}
	return fallback
}

// fmtNum is Python App._fmt_num: ints rounded, floats trimmed to ≤2 places.
func fmtNum(val float64, isFloat bool) string {
	if isFloat {
		s := strconv.FormatFloat(val, 'f', 2, 64)
		if strings.Contains(s, ".") {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		return s
	}
	return strconv.Itoa(int(math.Round(val)))
}

// editSlider is Python App._edit_slider: arrow stepping with live apply,
// Esc restoring the entry snapshot.
func (a *App) editSlider(opt *Option, lo, hi, step float64) {
	snap := a.snap()
	isFloat := opt.Kind == "float" || step < 1
	nsteps := max(1, int(math.Round((hi-lo)/step)))
	cur := a.sess.Effective(opt.Name)
	start := lo
	if cur != "" {
		if f, err := strconv.ParseFloat(cur, 64); err == nil {
			start = f
		}
	}
	idx := max(0, min(nsteps, int(math.Round((start-lo)/step))))
	value := func(i int) float64 {
		if i >= nsteps {
			return hi
		}
		return lo + float64(i)*step
	}

	pending := true
	for {
		a.drawSlider(opt, lo, hi, value(idx), isFloat, float64(idx)/float64(nsteps))
		if pending {
			_, _ = a.commitScalar(opt, fmtNum(value(idx), isFloat))
			pending = false
		}
		k := a.nextEvent(0)
		switch k.Kind {
		case keyEscape:
			a.restore(snap)
			a.msg("cancelled", "info")
			return
		case keyEnter:
			v := fmtNum(value(idx), isFloat)
			ok, errs := a.commitScalar(opt, v)
			if !ok {
				a.restore(snap)
			}
			a.report(opt, v, ok, errs)
			return
		}
		newIdx := idx
		switch {
		case k.Kind == keyLeft || k.Kind == keyDown ||
			(k.Kind == keyRune && strings.ContainsRune("-_hj", k.Rune)):
			newIdx = idx - 1
		case k.Kind == keyRight || k.Kind == keyUp ||
			(k.Kind == keyRune && strings.ContainsRune("+=lk", k.Rune)):
			newIdx = idx + 1
		case k.Kind == keyPgDn: // Python maps KEY_NPAGE to -10 here (sic)
			newIdx = idx - 10
		case k.Kind == keyPgUp: // ...and KEY_PPAGE to +10
			newIdx = idx + 10
		case k.Kind == keyHome:
			newIdx = 0
		case k.Kind == keyEnd:
			newIdx = nsteps
		}
		newIdx = max(0, min(nsteps, newIdx))
		if newIdx != idx {
			idx = newIdx
			pending = true
		}
	}
}

func (a *App) drawSlider(opt *Option, lo, hi, val float64, isFloat bool, frac float64) {
	a.newScreen()
	h, w := a.rows, a.cols
	a.fb.addstr(0, 0, ljust(" set · "+opt.Name+" ", w), textAttr{pair: 1, bold: true})
	row := 2
	for i, dl := range strings.Split(opt.Doc, "\n") {
		if i >= 3 {
			break
		}
		a.fb.addstr(row, 2, clipRunes(dl, w-3), textAttr{pair: 4})
		row++
	}

	loS, hiS, valS := fmtNum(lo, isFloat), fmtNum(hi, isFloat), fmtNum(val, isFloat)
	barW := max(10, min(48, w-(runeLen(loS)+runeLen(hiS)+8)))
	knob := max(0, min(barW-1, int(math.Round(frac*float64(barW-1)))))
	y := max(row+2, h/2-1)
	x := max(2, (w-(barW+runeLen(loS)+runeLen(hiS)+4))/2)

	a.fb.addstr(y, x, loS+" ", textAttr{pair: 4})
	bx := x + runeLen(loS) + 1
	a.fb.addstr(y, bx, strings.Repeat("━", knob), textAttr{pair: 5, bold: true})
	a.fb.addstr(y, bx+knob, "●", textAttr{pair: 6, bold: true})
	a.fb.addstr(y, bx+knob+1, strings.Repeat("─", barW-knob-1), textAttr{pair: 4})
	a.fb.addstr(y, bx+barW+1, " "+hiS, textAttr{pair: 4})

	vtxt := "  " + valS + "  "
	a.fb.addstr(y+2, max(2, (w-runeLen(vtxt))/2), vtxt, textAttr{pair: 10, bold: true})
	defaultLine := "default: " + orEmpty(opt.Default)
	a.fb.addstr(y+3, max(2, (w-runeLen(defaultLine))/2), defaultLine, textAttr{pair: 4})
	a.fb.addstr(h-1, 0,
		ljust(" ←/→ adjust · PgUp/PgDn ×10 · Home/End min/max · Enter apply · Esc cancel ", w),
		textAttr{pair: 1})
	a.flush()
}

func (a *App) editText(opt *Option) {
	snap := a.snap()
	cur := a.sess.Effective(opt.Name)
	var live func(string)
	if opt.isColor() {
		live = func(v string) { _, _ = a.commitScalar(opt, v) }
	}
	newVal, ok := a.lineEditor(opt.Name+" = ", cur, "Enter apply · Esc cancel", live)
	if !ok {
		a.restore(snap)
		a.msg("cancelled", "info")
		return
	}
	v := strings.TrimSpace(newVal)
	ok2, errs := a.commitScalar(opt, v)
	if !ok2 {
		a.restore(snap)
	}
	a.report(opt, v, ok2, errs)
}

func (a *App) editList(opt *Option) {
	values := append([]string(nil), a.sess.EffectiveList(opt.Name)...)
	sel := 0
	hintAdd := opt.Name + " entry"
	switch opt.Name {
	case "keybind":
		hintAdd = "trigger=action e.g. cmd+k=clear_screen"
	case "palette":
		hintAdd = "index=color e.g. 4=#89b4fa"
	case "env":
		hintAdd = "NAME=value"
	}
	for {
		a.newScreen()
		h, w := a.rows, a.cols
		a.fb.addstr(0, 0, ljust(" edit list · "+opt.Name+" ", w), textAttr{pair: 1, bold: true})
		a.fb.addstr(1, 0, strings.Split(opt.Doc, "\n")[0], textAttr{pair: 4})
		top := 3
		for i, v := range values {
			y := top + i
			if y >= h-2 {
				break
			}
			var attr textAttr
			if i == sel {
				attr = textAttr{pair: 3, bold: true}
			}
			a.fb.addstr(y, 2, fmt.Sprintf("%2d. ", i+1), attr)
			a.drawValueLine(y, 7, w-8, opt, v, i == sel)
		}
		if len(values) == 0 {
			a.fb.addstr(top, 2, "(empty — press 'a' to add)", textAttr{pair: 4})
		}
		a.fb.addstr(h-1, 0, ljust(" a add · e edit · d delete · Enter save · Esc cancel ", w),
			textAttr{pair: 1})
		a.flush()
		k := a.nextEvent(0)
		switch {
		case k.Kind == keyEscape:
			a.msg("cancelled", "info")
			return
		case k.Kind == keyEnter:
			ok, errs := a.commitList(opt, values)
			if ok {
				a.msg(fmt.Sprintf("%s: %d entries%s", opt.Name, len(values), a.liveTag()), "ok")
			} else {
				a.msg("invalid: "+firstErr(errs), "error")
			}
			return
		case k.Kind == keyUp || (k.Kind == keyRune && k.Rune == 'k'):
			sel = max(0, sel-1)
		case k.Kind == keyDown || (k.Kind == keyRune && k.Rune == 'j'):
			if len(values) > 0 {
				sel = min(len(values)-1, sel+1)
			} else {
				sel = 0
			}
		case k.Kind == keyRune && k.Rune == 'a':
			var nv string
			var ok bool
			if opt.Name == "keybind" {
				nv, ok = a.editKeybindForm("")
			} else {
				nv, ok = a.lineEditor("add › ", "", hintAdd, nil)
			}
			if ok && nv != "" { // Python `if nv:` — empty is falsy
				values = append(values, strings.TrimSpace(nv))
				sel = len(values) - 1
			}
		case k.Kind == keyRune && k.Rune == 'e' && len(values) > 0:
			var nv string
			var ok bool
			if opt.Name == "keybind" {
				nv, ok = a.editKeybindForm(values[sel])
			} else {
				nv, ok = a.lineEditor("edit › ", values[sel], hintAdd, nil)
			}
			if ok { // Python `if nv is not None:` — even empty replaces
				values[sel] = strings.TrimSpace(nv)
			}
		case (k.Kind == keyRune && k.Rune == 'd' || k.Kind == keyDelete) && len(values) > 0:
			values = append(values[:sel], values[sel+1:]...)
			sel = max(0, min(sel, len(values)-1))
		}
	}
}

// ---- Keybind builder ----

// keybindState is the mutable form state of the keybind builder.
type keybindState struct {
	mods   map[string]bool
	key    string
	action string
	args   string
}

// assembleKeybind builds the trigger=action string (Python App._assemble_keybind).
func assembleKeybind(st *keybindState) string {
	var mods []string
	for _, m := range keybindMods {
		if st.mods[m] {
			mods = append(mods, m)
		}
	}
	parts := mods
	if st.key != "" {
		parts = append(append([]string(nil), mods...), st.key)
	}
	action := st.action
	if action != "" && strings.TrimSpace(st.args) != "" {
		action = action + ":" + strings.TrimSpace(st.args)
	}
	return strings.Join(parts, "+") + "=" + action
}

// parseKeybindInto seeds the form from an existing binding
// (Python App._parse_keybind_into).
func parseKeybindInto(initial string, st *keybindState) {
	trig, act, _ := strings.Cut(initial, "=")
	var parts []string
	for _, p := range strings.Split(trig, "+") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) > 0 {
		st.key = strings.TrimSpace(parts[len(parts)-1])
		for _, m := range parts[:len(parts)-1] {
			mm := strings.ToLower(strings.TrimSpace(m))
			if alias, ok := keybindModAliases[mm]; ok {
				mm = alias
			}
			if _, ok := st.mods[mm]; ok {
				st.mods[mm] = true
			}
		}
	}
	action, args, _ := strings.Cut(strings.TrimSpace(act), ":")
	st.action = strings.TrimSpace(action)
	st.args = strings.TrimSpace(args)
}

// editKeybindForm is the guided keybind builder (Python App._edit_keybind_form).
// Returns the validated trigger=action string, or ("", false) on Esc.
func (a *App) editKeybindForm(initial string) (string, bool) {
	st := &keybindState{mods: map[string]bool{}}
	for _, m := range keybindMods {
		st.mods[m] = false
	}
	if initial != "" {
		parseKeybindInto(initial, st)
	}
	row, modsel, errmsg := 0, 0, ""
	const nrows = 5
	for {
		a.drawKeybindForm(st, row, modsel, errmsg)
		k := a.nextEvent(0)
		if k.Kind == keyEscape {
			return "", false
		}
		if k.Kind == keyDown || k.Kind == keyTab {
			row = (row + 1) % nrows
			continue
		}
		if k.Kind == keyUp || k.Kind == keyShiftTab {
			row = (row - 1 + nrows) % nrows
			continue
		}
		errmsg = ""
		switch row {
		case 0: // modifiers
			switch k.Kind {
			case keyLeft:
				modsel = (modsel - 1 + len(keybindMods)) % len(keybindMods)
			case keyRight:
				modsel = (modsel + 1) % len(keybindMods)
			case keyRune:
				if k.Rune == ' ' {
					m := keybindMods[modsel]
					st.mods[m] = !st.mods[m]
				}
			}
		case 1: // key
			switch {
			case k.Kind == keyEnter:
				if pick, ok := a.picker("special key", keybindNamedKeys, st.key, nil, nil); ok {
					st.key = pick
				}
			case k.Kind == keyBackspace || k.Kind == keyDelete:
				st.key = ""
			case k.Kind == keyRune && k.Rune != ' ': // Python `32 < ch < 127`
				r := k.Rune
				if unicode.IsLetter(r) {
					r = unicode.ToLower(r)
				}
				st.key = string(r)
			}
		case 2: // action
			if k.Kind == keyEnter {
				acts := listActions()
				if len(acts) > 0 {
					if pick, ok := a.picker("action", acts, st.action, nil, nil); ok {
						st.action = pick
					}
				} else {
					errmsg = "could not load Ghostty actions"
				}
			}
		case 3: // args
			switch k.Kind {
			case keyBackspace:
				st.args = trimLastRune(st.args)
			case keyEnter:
				row = 4
			case keyRune:
				st.args += string(k.Rune)
			}
		case 4: // save
			if k.Kind == keyEnter {
				switch {
				case st.key == "":
					errmsg, row = "pick a key first", 1
					continue
				case st.action == "":
					errmsg, row = "pick an action first", 2
					continue
				}
				result := assembleKeybind(st)
				if ok, _ := validateConfig("keybind = " + result + "\n"); !ok {
					errmsg = "Ghostty rejected: " + result
					continue
				}
				return result, true
			}
		}
	}
}

func (a *App) drawKeybindForm(st *keybindState, row, modsel int, errmsg string) {
	a.newScreen()
	h, w := a.rows, a.cols
	a.fb.addstr(0, 0, ljust(" build keybind ", w), textAttr{pair: 1, bold: true})
	y := 2
	a.fb.addstr(y, 2, "Modifiers:", textAttr{pair: 4})
	x := 14
	for i, m := range keybindMods {
		box := "[ ]"
		if st.mods[m] {
			box = "[x]"
		}
		label := box + " " + m
		attr := textAttr{pair: 5}
		if row == 0 && i == modsel {
			attr = textAttr{pair: 3, bold: true}
		}
		a.fb.addstr(y, x, label, attr)
		x += runeLen(label) + 2
	}
	if isMacOS {
		a.fb.addstr(y+1, 14, "(super = ⌘ Command on macOS)", textAttr{pair: 4})
	}
	y += 3

	field := func(label, val string, r int, hint string) {
		attr := textAttr{pair: 5}
		if row == r {
			attr = textAttr{pair: 3, bold: true}
		}
		a.fb.addstr(y, 2, ljust(label, 9), textAttr{pair: 4})
		a.fb.addstr(y, 12, clipRunes(orEmptyDash(val), 24), attr)
		a.fb.addstr(y, 40, hint, textAttr{pair: 4})
	}
	field("Key:", st.key, 1, "type a key · Enter → named-key list")
	y++
	field("Action:", st.action, 2, "Enter → choose from Ghostty actions")
	y++
	field("Args:", st.args, 3, "optional, e.g. 1  or  mixed")
	y += 2

	a.fb.addstr(y, 2, "Result: ", textAttr{pair: 4})
	a.fb.addstr(y, 10, assembleKeybind(st), textAttr{pair: 6, bold: true})
	y += 2
	saveAttr := textAttr{pair: 6}
	if row == 4 {
		saveAttr = textAttr{pair: 3, bold: true}
	}
	a.fb.addstr(y, 2, "▸ Save this binding", saveAttr)
	y++
	if errmsg != "" {
		a.fb.addstr(y+1, 2, "⚠ "+errmsg, textAttr{pair: 7, bold: true})
	}
	a.fb.addstr(h-1, 0,
		ljust(" Tab/↑↓ field · ←/→ pick mod · Space toggle · Enter pick/save · Esc cancel ", w),
		textAttr{pair: 1})
	a.flush()
}

// orEmptyDash is Python's `(val or "—")` in the keybind form fields.
func orEmptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---- Widgets: prompt bar, line editor, picker ----

// promptBar draws the bottom edit line with a visible cursor
// (Python App._prompt_bar). Like curses it is an incremental update: the
// frame only rewrites the bottom two rows, no erase.
func (a *App) promptBar(label, buf, hint string) {
	a.fb = newFrameBuffer(a.rows, a.cols)
	h, w := a.rows, a.cols
	a.fb.addstr(h-2, 0, strings.Repeat(" ", w-1), textAttr{})
	a.fb.addstr(h-2, 0, label+buf, textAttr{pair: 5, bold: true})
	a.fb.addstr(h-1, 0, ljust(" "+hint, w), textAttr{pair: 1})
	a.fb.showCursor(h-2, min(w-1, runeLen(label)+runeLen(buf)))
	a.flush()
}

// lineEditor is Python App._line_editor: one-line input on the bottom row,
// ASCII only, optional live callback after each change. Returns (buf, true)
// on Enter, ("", false) on Esc.
func (a *App) lineEditor(label, initial, hint string, live func(string)) (string, bool) {
	buf := initial
	for {
		a.promptBar(label, buf, hint)
		k := a.nextEvent(0)
		switch k.Kind {
		case keyEscape:
			return "", false
		case keyEnter:
			return buf, true
		case keyBackspace:
			buf = trimLastRune(buf)
		case keyDelete:
			// pass (Python: no-op, but live still fires)
		case keyRune:
			buf += string(k.Rune)
		default:
			continue
		}
		if live != nil {
			live(buf)
		}
	}
}

// picker is Python App._picker: scrollable, type-to-filter list with a 90ms
// debounce timer driving live preview when auto-apply is on. side, when
// non-nil, draws a panel to the right of the list.
func (a *App) picker(title string, items []string, current string,
	preview func(string), side func(item string, x, y, width int)) (string, bool) {
	query := ""
	filtered := append([]string(nil), items...)
	sel := 0
	for i, it := range filtered {
		if it == current {
			sel = i
			break
		}
	}
	lastPreview := ""
	havePreviewed := false
	lastMove := time.Now()
	for {
		if preview != nil && a.sess.AutoApply && len(filtered) > 0 {
			want := filtered[sel]
			if (!havePreviewed || want != lastPreview) && time.Since(lastMove) > 110*time.Millisecond {
				preview(want)
				lastPreview = want
				havePreviewed = true
			}
		}
		a.drawPicker(title, query, filtered, sel, current, side)
		k := a.nextEvent(90 * time.Millisecond)
		if k == resizeKey { // timeout or resize: loop around (Python `ch == -1`)
			continue
		}
		lastMove = time.Now()
		switch k.Kind {
		case keyEscape:
			return "", false
		case keyEnter:
			if len(filtered) > 0 {
				return filtered[sel], true
			}
			return "", false
		case keyUp:
			sel = max(0, sel-1)
		case keyDown:
			if len(filtered) > 0 {
				sel = min(len(filtered)-1, sel+1)
			} else {
				sel = 0
			}
		case keyPgDn:
			if len(filtered) > 0 {
				sel = min(len(filtered)-1, sel+10)
			} else {
				sel = 0
			}
		case keyPgUp:
			sel = max(0, sel-10)
		case keyBackspace:
			query = trimLastRune(query)
			filtered = filterItems(items, query)
			sel = 0
		case keyRune:
			query += string(k.Rune)
			filtered = filterItems(items, query)
			sel = 0
		}
	}
}

func (a *App) drawPicker(title, query string, filtered []string, sel int,
	current string, side func(item string, x, y, width int)) {
	a.newScreen()
	h, w := a.rows, a.cols
	a.fb.addstr(0, 0, ljust(fmt.Sprintf(" select %s  (%d) ", title, len(filtered)), w),
		textAttr{pair: 1, bold: true})
	a.fb.addstr(1, 0, " filter: "+query, textAttr{pair: 5, bold: true})
	top := 3
	rows := h - 5
	sideX := -1
	listW := w
	if side != nil && w >= 56 {
		listW = w / 2
		sideX = listW + 2
	}
	scroll := max(0, sel-rows+1)
	for r := 0; r < rows; r++ {
		i := scroll + r
		if i >= len(filtered) {
			break
		}
		item := filtered[i]
		y := top + r
		isSel := i == sel
		var attr textAttr
		if isSel {
			attr = textAttr{pair: 3, bold: true}
		}
		marker := "  "
		if isSel {
			marker = "→ "
		} else if item == current {
			marker = "• "
		}
		hexPart := item
		if strings.Contains(item, "=") {
			hexPart = item[strings.LastIndex(item, "=")+1:]
		}
		x := 2
		a.fb.addstr(y, x, marker, attr)
		if _, _, _, ok := parseHex(hexPart); ok {
			sw := attr
			if idx, ok2 := hexTo256(hexPart); a.color256 && ok2 {
				sw = textAttr{fg256: idx}
			}
			a.fb.addstr(y, x+2, "██ ", sw)
			a.fb.addstr(y, x+5, clipRunes(item, listW-x-6), attr)
		} else {
			a.fb.addstr(y, x+2, clipRunes(item, listW-x-3), attr)
		}
	}
	if sideX >= 0 && len(filtered) > 0 {
		side(filtered[sel], sideX, top, w-sideX-1)
	}
	hint := " type to filter · ↑↓ move · Enter select · Esc cancel "
	if a.sess.AutoApply {
		hint = " ● LIVE PREVIEW ·" + hint
	}
	a.fb.addstr(h-1, 0, ljust(hint, w), textAttr{pair: 1})
	a.flush()
}

// drawThemeCard is the right-hand panel in the theme picker
// (Python App._draw_theme_card).
func (a *App) drawThemeCard(name string, x, y, width int) {
	a.fb.addstr(y, x, clipRunes(name, width), textAttr{pair: 10, bold: true})
	colors := a.effectiveColors(name, true)
	used := a.drawColorPreview(y+2, x, width, colors)
	if used == 0 {
		hint := "(no colour data for this theme)"
		if !a.color256 {
			hint = "(256-colour terminal needed for preview)"
		}
		a.fb.addstr(y+2, x, clipRunes(hint, width), textAttr{pair: 4})
		return
	}
	yy := y + 2 + used + 1
	a.fb.addstr(yy, x, clipRunes(fmt.Sprintf("bg %s   fg %s", colors.bg, colors.fg), width),
		textAttr{pair: 4})
}

// ---- Overlays ----

func (a *App) changesOverlay() {
	ovr := a.sess.Overrides()
	a.newScreen()
	h, w := a.rows, a.cols
	a.fb.addstr(0, 0, ljust(fmt.Sprintf(" changed from default · %d option(s) ", len(ovr)), w),
		textAttr{pair: 1, bold: true})
	y := 2
	if len(ovr) == 0 {
		a.fb.addstr(2, 2, "nothing changed — all defaults", textAttr{pair: 4})
	}
	for _, o := range ovr {
		if y >= h-2 {
			a.fb.addstr(h-2, 2, "…", textAttr{pair: 4})
			break
		}
		a.fb.addstr(y, 2, o.Name, textAttr{pair: 10, bold: true})
		a.fb.addstr(y, 34, clipRunes("= "+o.Value, w-36), textAttr{pair: 5})
		y++
	}
	a.fb.addstr(h-1, 0, ljust(fmt.Sprintf(" config: %s  ·  any key to close ", a.sess.Cfg.Path), w),
		textAttr{pair: 1})
	a.flush()
	a.nextEvent(0)
}

func (a *App) profilesOverlay() {
	sel := 0
	for {
		profiles := listProfiles()
		if len(profiles) > 0 {
			sel = max(0, min(sel, len(profiles)-1))
		} else {
			sel = 0
		}
		a.newScreen()
		h, w := a.rows, a.cols
		a.fb.addstr(0, 0, ljust(" profiles ", w), textAttr{pair: 1, bold: true})
		a.fb.addstr(1, 0, clipRunes(" saved in "+profilesDir(), w), textAttr{pair: 4})
		top := 3
		if len(profiles) == 0 {
			a.fb.addstr(top, 2, "(no profiles yet — press 's' to save the current config)",
				textAttr{pair: 4})
		}
		for i, p := range profiles {
			y := top + i
			if y >= h-2 {
				break
			}
			attr := textAttr{}
			marker := "  "
			if i == sel {
				attr = textAttr{pair: 3, bold: true}
				marker = "→ "
			}
			a.fb.addstr(y, 2, marker+p, attr)
		}
		a.fb.addstr(h-1, 0, ljust(" s save · Enter/l load · d delete · t light↔dark · Esc close ", w),
			textAttr{pair: 1})
		a.flush()
		k := a.nextEvent(0)
		switch {
		case k.Kind == keyEscape:
			return
		case k.Kind == keyUp || (k.Kind == keyRune && k.Rune == 'k'):
			sel = max(0, sel-1)
		case k.Kind == keyDown || (k.Kind == keyRune && k.Rune == 'j'):
			sel = min(max(0, len(profiles)-1), sel+1)
		case k.Kind == keyRune && k.Rune == 's':
			name, ok := a.lineEditor("save profile as › ", "",
				"name (letters/numbers/._-) · Enter save · Esc cancel", nil)
			if ok && name != "" { // Python `if name:`
				ok2, m := a.sess.SaveProfile(name)
				a.msg(m, kindFor(ok2))
			}
		case (k.Kind == keyRune && k.Rune == 'd' || k.Kind == keyDelete) && len(profiles) > 0:
			if a.confirm(fmt.Sprintf("Delete profile '%s'?", profiles[sel])) {
				ok2, m := a.sess.DeleteProfile(profiles[sel])
				a.msg(m, kindFor(ok2))
			}
		case k.Kind == keyRune && k.Rune == 't':
			ok2, m := a.sess.ToggleLightDark()
			if ok2 {
				a.msg(m, "ok")
				return
			}
			a.msg(m, "warn")
		case (k.Kind == keyEnter || (k.Kind == keyRune && k.Rune == 'l')) && len(profiles) > 0:
			if a.confirm(fmt.Sprintf("Load '%s'? (current config backed up)", profiles[sel])) {
				ok2, m := a.sess.LoadProfile(profiles[sel])
				a.msg(m, kindFor(ok2))
				if ok2 {
					return
				}
			}
		}
	}
}

func (a *App) doctorOverlay() {
	a.msg("running config check…", "info")
	a.draw()
	findings := runDoctor(a.sess)
	colors := map[string]int{"error": 7, "warn": 8, "info": 4, "ok": 6}
	icons := map[string]string{"error": "✗", "warn": "!", "info": "·", "ok": "✓"}
	scroll := 0
	for {
		a.newScreen()
		h, w := a.rows, a.cols
		nErr, nWarn := 0, 0
		for _, f := range findings {
			switch f.Severity {
			case "error":
				nErr++
			case "warn":
				nWarn++
			}
		}
		a.fb.addstr(0, 0, ljust(fmt.Sprintf(" config check · %d error(s), %d warning(s) ", nErr, nWarn), w),
			textAttr{pair: 1, bold: true})
		type wline struct {
			sev  string
			text string
		}
		var wrapped []wline
		for _, f := range findings {
			for j, seg := range wrapText(f.Message, w-6) {
				prefix := "  "
				if j == 0 {
					prefix = icons[f.Severity] + " "
				}
				wrapped = append(wrapped, wline{f.Severity, prefix + seg})
			}
		}
		rows := h - 3
		for r := 0; r < rows; r++ {
			i := scroll + r
			if i >= len(wrapped) {
				break
			}
			wl := wrapped[i]
			attr := textAttr{pair: colors[wl.sev]}
			if wl.sev == "error" || wl.sev == "warn" {
				attr.bold = true
			}
			a.fb.addstr(2+r, 2, clipRunes(wl.text, w-3), attr)
		}
		a.fb.addstr(h-1, 0, ljust(" ↑↓ scroll · any other key to close ", w), textAttr{pair: 1})
		a.flush()
		k := a.nextEvent(0)
		switch k.Kind {
		case keyDown:
			scroll = min(max(0, len(wrapped)-rows), scroll+1)
		case keyUp:
			scroll = max(0, scroll-1)
		case keyPgDn:
			scroll = min(max(0, len(wrapped)-rows), scroll+rows)
		case keyPgUp:
			scroll = max(0, scroll-rows)
		default:
			return
		}
	}
}

// ---- Utils category & overlay ----

// utilEntry is one one-shot maintenance action (Python App._utils entries).
type utilEntry struct {
	name    string
	explain []string
	status  func() string
	run     func() (bool, string)
}

// utilsList is the Utils menu registry (Python App._utils).
func (a *App) utilsList() []utilEntry {
	return []utilEntry{{
		name:    "Fix SSH",
		explain: sshFixExplanation,
		status: func() string {
			if p := findSSHAlias(); p != "" {
				return "applied · alias in " + tilde(p)
			}
			return "not applied yet"
		},
		run: applySSHFix,
	}}
}

func (a *App) drawUtilsMenu(top, bottom, catW, optW int) {
	x0 := catW + 1
	utils := a.utilsList()
	for i, u := range utils {
		y := top + i
		if y > bottom {
			break
		}
		a.fb.addstr(y, x0, clipRunes("• "+u.name, optW-2), textAttr{pair: 5, bold: true})
	}
	if y := top + len(utils) + 1; y <= bottom {
		a.fb.addstr(y, x0, "Enter → open", textAttr{pair: 6})
	}
}

func (a *App) drawUtilsDetail(top, bottom, x, width int) {
	if width < 10 {
		return
	}
	y := top
	a.fb.addstr(y, x, "Utils", textAttr{pair: 10, bold: true})
	y++
	a.fb.addstr(y, x, "one-shot maintenance actions", textAttr{pair: 2})
	y += 2
	a.fb.addstr(y, x, "Press → or Enter to open the Utils menu.", textAttr{pair: 4})
	y += 2
	utils := a.utilsList()
	if len(utils) == 0 {
		return
	}
	u := utils[0]
	a.fb.addstr(y, x, ("─ "+u.name+" ")+strings.Repeat("─", max(0, width-runeLen(u.name)-3)),
		textAttr{pair: 4})
	y++
	if status := u.status(); status != "" {
		a.fb.addstr(y, x, clipRunes("status: "+status, width), textAttr{pair: 6})
		y++
	}
	if len(u.explain) > 2 {
		for _, ln := range u.explain[2:] {
			done := false
			for _, seg := range wrapText(ln, width) {
				if y > bottom {
					done = true
					break
				}
				attr := textAttr{pair: 4}
				if ln != "" && !strings.HasPrefix(ln, " ") && !strings.Contains(ln, ":") {
					attr = textAttr{pair: 5, bold: true}
				}
				a.fb.addstr(y, x, clipRunes(seg, width), attr)
				y++
			}
			if done {
				break
			}
		}
	}
}

func (a *App) utilsOverlay() {
	utils := a.utilsList()
	sel := 0
	for {
		a.newScreen()
		h, w := a.rows, a.cols
		a.fb.addstr(0, 0, ljust(" utils · one-shot fixes ", w), textAttr{pair: 1, bold: true})
		listW := 20
		top := 2
		for i, u := range utils {
			y := top + i
			if y >= h-2 {
				break
			}
			attr := textAttr{}
			marker := "  "
			if i == sel {
				attr = textAttr{pair: 3, bold: true}
				marker = "→ "
			}
			a.fb.addstr(y, 2, marker+u.name, attr)
		}
		for y := top; y < h-2; y++ {
			a.fb.addstr(y, listW, "│", textAttr{pair: 4})
		}

		u := utils[sel]
		dx, dw := listW+2, w-listW-3
		y := top
		if status := u.status(); status != "" {
			a.fb.addstr(y, dx, clipRunes("status: "+status, dw), textAttr{pair: 6})
			y += 2
		}
	Explain:
		for _, ln := range u.explain {
			for _, seg := range wrapText(ln, dw) {
				if y >= h-2 {
					break Explain
				}
				attr := textAttr{pair: 4}
				if ln != "" && !strings.HasPrefix(ln, " ") && !strings.Contains(ln, ":") {
					attr = textAttr{pair: 5, bold: true}
				}
				a.fb.addstr(y, dx, clipRunes(seg, dw), attr)
				y++
			}
		}
		a.fb.addstr(h-1, 0, ljust(" ↑↓ move · Enter run · Esc close ", w), textAttr{pair: 1})
		a.flush()
		k := a.nextEvent(0)
		switch {
		case k.Kind == keyEscape:
			return
		case k.Kind == keyUp || (k.Kind == keyRune && k.Rune == 'k'):
			sel = max(0, sel-1)
		case k.Kind == keyDown || (k.Kind == keyRune && k.Rune == 'j'):
			sel = min(len(utils)-1, sel+1)
		case k.Kind == keyEnter:
			if a.confirm(fmt.Sprintf("Run '%s' now?", u.name)) {
				a.msg(fmt.Sprintf("running %s…", u.name), "info")
				a.draw()
				ok, m := u.run()
				a.utilsResult(u.name, ok, m)
			}
		}
	}
}

func (a *App) utilsResult(name string, ok bool, message string) {
	a.newScreen()
	h, w := a.rows, a.cols
	pair := 7
	if ok {
		pair = 6
	}
	a.fb.addstr(0, 0, ljust(" "+name+" ", w), textAttr{pair: pair, bold: true})
	y := 2
	mark := "✗ "
	if ok {
		mark = "✓ "
	}
	for _, seg := range wrapText(mark+message, w-4) {
		if y >= h-2 {
			break
		}
		a.fb.addstr(y, 2, clipRunes(seg, w-3), textAttr{pair: pair, bold: true})
		y++
	}
	a.fb.addstr(h-1, 0, ljust(" any key to return ", w), textAttr{pair: 1})
	a.flush()
	a.nextEvent(0)
}

// help is Python App._help: the key reference overlay.
func (a *App) help() {
	var updateLine string
	if info := a.updateInfo(); info != nil && info.Outdated {
		updateLine = fmt.Sprintf("A newer release (%s) is available at %s", info.Latest, info.URL)
	} else {
		updateLine = fmt.Sprintf("You're on the latest version (v%s).", version)
	}
	lines := []string{
		"SpookiUI v" + version + " — live Ghostty configurator",
		"",
		"Navigation",
		"  ↑/↓ or j/k    move            Tab      switch pane",
		"  →/Enter/l     into options / edit an option",
		"  ←/h           back to categories",
		"  PgUp/PgDn     jump by 10",
		"  /             search all options by name or docs",
		"",
		"Editing (changes apply LIVE when auto-apply is on)",
		"  Enter         edit the selected option",
		"   • booleans toggle instantly",
		"   • enums/theme/font open a picker with live preview",
		"   • theme picker shows a live colour card for each theme",
		"   • numbers: ↑↓ or +/- to step, or type a value",
		"   • bounded values (opacity, contrast): a slider — ←/→ to adjust",
		"   • colors/text: type a value (#hex or name); colours preview",
		"   • lists (palette/env): a add, e edit, d delete",
		"   • keybind: a/e open a builder — toggle modifiers, pick an action",
		"  u             reset the selected option to its default",
		"",
		"Session",
		"  a   toggle auto-apply (live vs. staged)",
		"  s   save + reload now      r   re-trigger reload",
		"  R   revert everything to session start",
		"  X   wipe config & restore all Ghostty defaults (backup kept)",
		"  U   update SpookiUI in place to the latest release",
		"  p   profiles — save / load / delete named configs, light↔dark",
		"  c   config check — health-check for issues (doctor)",
		"  v   utils — one-shot fixes (e.g. Fix SSH for garbled remote shells)",
		"  d   show what you've changed",
		"  q   quit",
		"",
		"Options that only apply to the other OS are hidden automatically.",
		"Category icons need a Nerd Font terminal font (SPOOKIUI_ICONS=1 forces them).",
		"",
		"Live reload works by clicking Ghostty's 'Reload Configuration'",
		"menu item on macOS, or sending it SIGUSR2 on Linux. A timestamped",
		"backup of your config is made on the first change of each session.",
		"",
		updateLine,
	}
	a.newScreen()
	h, w := a.rows, a.cols
	a.fb.addstr(0, 0, ljust(" help ", w), textAttr{pair: 1, bold: true})
	for i, ln := range lines {
		if i+2 >= h-1 {
			break
		}
		attr := textAttr{pair: 4}
		if ln != "" && !strings.HasPrefix(ln, " ") && !strings.Contains(ln, ":") &&
			unicode.IsUpper([]rune(ln)[0]) && !strings.Contains(ln, "—") {
			attr = textAttr{pair: 5, bold: true}
		}
		a.fb.addstr(i+2, 2, clipRunes(ln, w-3), attr)
	}
	a.fb.addstr(h-1, 0, ljust(" any key to close ", w), textAttr{pair: 1})
	a.flush()
	a.nextEvent(0)
}

// confirm asks a y/N question on the bottom row (Python App._confirm).
// Incremental like _prompt_bar: only the bottom two rows are rewritten.
func (a *App) confirm(question string) bool {
	a.fb = newFrameBuffer(a.rows, a.cols)
	h, w := a.rows, a.cols
	a.fb.addstr(h-2, 0, ljust(" "+question+"  [y/N] ", w), textAttr{pair: 8, bold: true})
	a.fb.addstr(h-1, 0, ljust(" ", w), textAttr{pair: 1})
	a.flush()
	k := a.nextEvent(0)
	return k.Kind == keyRune && (k.Rune == 'y' || k.Rune == 'Y')
}

// quit is Python App._quit: offer to save staged changes, then leave the loop.
func (a *App) quit() bool {
	if a.sess.Dirty && !a.sess.AutoApply {
		if a.confirm("You have unsaved changes. Save before quitting?") {
			ok, m := a.sess.Apply()
			a.msg(m, kindFor(ok))
		}
	}
	return false
}

// ---- runTUI: wiring (Python main() no-subcommand path + run_tui) ----

// isATTY is Python's sys.stdout.isatty() gate for launching the TUI.
func isATTY(f *os.File) bool {
	_, err := tcGetAttr(f.Fd())
	return err == nil
}

// runTUI launches the interactive UI: refuse without a tty (Python main()),
// show the one-time icon notice when icons are unavailable, then drive the
// App over the terminal layer until quit (Python curses.wrapper + App.run;
// KeyboardInterrupt maps to our keyCtrlC path).
func runTUI(sess *Session) int {
	if !isATTY(os.Stdout) {
		fmt.Fprintln(os.Stderr, "Refusing to launch the TUI without a terminal. "+
			"Run `spookiui --help` for the scriptable CLI.")
		return 1
	}
	icons := iconsAvailable(sess)
	if !icons {
		maybeShowIconNotice(os.Stdout, os.Stdin)
	}
	term, err := openTerm(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer term.Close()
	kr := newKeyReader(os.Stdin, fdWaitMore(int(os.Stdin.Fd())))
	kr.start()
	mux := newEventMux(kr, term.Close)
	defer mux.stop()

	app := newApp(sess, icons, term.Color256())
	app.rows, app.cols = term.Size()
	app.keys = mux.Keys
	app.resize = mux.Resize
	app.sizeFn = term.Size
	app.flushFn = func(fb *frameBuffer) {
		//nolint:errcheck // best-effort frame write; a dead tty ends input too
		_ = term.WriteFrame(fb.render())
	}
	app.run()
	return 0
}

// runCLI is main()'s logic with injectable streams. Exit codes: 0 ok,
// 1 generic, 2 usage/unknown key, 3 ghostty not found.
func runCLI(argv []string, stdout, stderr io.Writer) int {
	// argparse handles -V/--version and -h/--help before anything else.
	if len(argv) > 0 {
		switch argv[0] {
		case "-h", "--help", "help":
			fmt.Fprint(stdout, helpText())
			return 0
		case "-V", "--version":
			fmt.Fprintf(stdout, "SpookiUI v%s\n", version)
			return 0
		}
	}

	if ghosttyPath == "" {
		fmt.Fprintln(stderr, "error: could not find the `ghostty` executable.")
		fmt.Fprintln(stderr, "Install Ghostty or add it to your PATH.")
		return 3
	}

	sess, err := NewSession()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 3
	}

	if len(argv) == 0 {
		return runTUI(sess)
	}

	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "list":
		return cliList(sess, rest, stdout, stderr)
	case "get":
		return cliGet(sess, rest, stdout, stderr)
	case "doc":
		return cliDoc(sess, rest, stdout, stderr)
	case "set":
		return cliSet(sess, rest, stdout, stderr)
	case "version":
		return cliVersion(sess, rest, stdout, stderr)
	case "update":
		return cliUpdate(sess, rest, stdout, stderr)
	case "reset":
		return cliReset(sess, rest, stdout, stderr)
	case "reload":
		return cliReload(sess, rest, stdout, stderr)
	case "validate":
		return cliValidate(sess, rest, stdout, stderr)
	case "themes":
		return cliThemes(sess, rest, stdout, stderr)
	case "fonts":
		return cliFonts(sess, rest, stdout, stderr)
	case "path":
		return cliPath(sess, rest, stdout, stderr)
	case "profile":
		return cliProfile(sess, rest, stdout, stderr)
	case "doctor":
		return cliDoctor(sess, rest, stdout, stderr)
	case "fix-ssh":
		return cliFixSSH(sess, rest, stdout, stderr)
	default:
		return cliUsageError(stderr, "unknown command: %s (choose from list, get, doc, set, "+
			"version, update, reset, reload, validate, themes, fonts, path, profile, doctor, fix-ssh)", cmd)
	}
}

func main() {
	// Python sets the C locale here for curses; Go needs no locale setup.
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
