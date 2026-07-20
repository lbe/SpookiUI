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

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"1.10.2", []int{1, 10, 2}},
		{"v2.0.0", []int{2, 0, 0}},
		{"V3.1", []int{3, 1}},
		{"2.0.0-beta", []int{2, 0, 0}},
		{"1.2+build5", []int{1, 2}},
		{"1.2", []int{1, 2}},
		{"", []int{0}},
		{"v", []int{0}},
		{" 1.2.3 ", []int{1, 2, 3}},
		{"no-numbers", []int{0}},
	}
	for _, c := range cases {
		got := parseVersion(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"1.10.2", "1.2.0", true},
		{"1.2.0", "1.10.2", false},
		{"1.2.0", "1.2", true}, // Python tuple compare: (1,2,0) > (1,2)
		{"1.2", "1.2.0", false},
		{"2.0.0-beta", "2.0.0", false},
		{"v2.0.0", "2.0.0", false},
		{"2.0.0", "2.0.0", false},
		{"", "0.0.1", false}, // "" parses to (0,) < (0,0,1)
	}
	for _, c := range cases {
		if got := isNewer(c.latest, c.current); got != c.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestUpdateCheckDisabled(t *testing.T) {
	for _, c := range []struct {
		val  string
		want bool
	}{
		{"", false}, {"0", false}, {" 0 ", false},
		{"1", true}, {"yes", true}, {" 1 ", true},
	} {
		t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", c.val)
		if got := updateCheckDisabled(); got != c.want {
			t.Errorf("updateCheckDisabled() with %q = %v, want %v", c.val, got, c.want)
		}
	}
}

// stubFetch replaces fetchLatestRelease for the duration of a test.
func stubFetch(t *testing.T, fn func(time.Duration) *releaseInfo) {
	t.Helper()
	orig := fetchLatestRelease
	fetchLatestRelease = fn
	t.Cleanup(func() { fetchLatestRelease = orig })
}

func TestCheckForUpdateCacheTTL(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "")
	fetches := 0
	stubFetch(t, func(time.Duration) *releaseInfo {
		fetches++
		return &releaseInfo{Latest: "v99.0.0", URL: "https://example.invalid/rel", Notes: "some notes"}
	})
	now := 1_000_000.0

	info := checkForUpdate(false, now)
	if info == nil {
		t.Fatal("expected update info, got nil")
	}
	if info.Latest != "v99.0.0" || !info.Outdated || info.Current != version {
		t.Errorf("unexpected info: %+v", info)
	}
	if info.URL != "https://example.invalid/rel" || info.Notes != "some notes" {
		t.Errorf("unexpected url/notes: %+v", info)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}

	// Within the 24h TTL the cache is served, no new fetch.
	info = checkForUpdate(false, now+3600)
	if info == nil || info.Latest != "v99.0.0" {
		t.Fatalf("cached call returned %+v", info)
	}
	if fetches != 1 {
		t.Fatalf("fetches after cached call = %d, want 1", fetches)
	}

	// force bypasses the cache.
	checkForUpdate(true, now+3600)
	if fetches != 2 {
		t.Fatalf("fetches after forced call = %d, want 2", fetches)
	}

	// Past the TTL it fetches again.
	checkForUpdate(false, now+25*3600)
	if fetches != 3 {
		t.Fatalf("fetches after TTL expiry = %d, want 3", fetches)
	}
}

func TestCheckForUpdateDisabledReturnsNil(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "1")
	called := false
	stubFetch(t, func(time.Duration) *releaseInfo {
		called = true
		return &releaseInfo{Latest: "v99.0.0"}
	})
	if info := checkForUpdate(false, 123.0); info != nil {
		t.Errorf("disabled check returned %+v, want nil", info)
	}
	if called {
		t.Error("fetch was called despite opt-out")
	}
	// force overrides the opt-out.
	if info := checkForUpdate(true, 123.0); info == nil {
		t.Error("forced check returned nil, want info")
	}
}

func TestCheckForUpdateFetchFailure(t *testing.T) {
	t.Setenv("SPOOKIUI_NO_UPDATE_CHECK", "")
	stubFetch(t, func(time.Duration) *releaseInfo { return nil })

	// No cache + failed fetch -> nil.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if info := checkForUpdate(false, 500.0); info != nil {
		t.Errorf("no-cache failed fetch returned %+v, want nil", info)
	}

	// Stale cache + failed fetch -> cached info.
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	cachePath := filepath.Join(dir, "spookiui", "update-check.json")
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath,
		[]byte(`{"checked_at": 0, "latest": "v8.0.0", "url": "https://x", "notes": "n"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	info := checkForUpdate(false, 999999999.0) // way past TTL
	if info == nil {
		t.Fatal("stale-cache failed fetch returned nil, want cached info")
	}
	if info.Latest != "v8.0.0" || info.URL != "https://x" || info.Notes != "n" {
		t.Errorf("unexpected cached info: %+v", info)
	}
	if !info.Outdated {
		t.Error("expected outdated=true for v8.0.0 vs current")
	}

	// Corrupt cache file behaves like no cache.
	if err := os.WriteFile(cachePath, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := checkForUpdate(false, 999999999.0); info != nil {
		t.Errorf("corrupt-cache failed fetch returned %+v, want nil", info)
	}
}
