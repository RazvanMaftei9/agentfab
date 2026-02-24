package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// resetToolchainCache clears the sync.Once cache so each test starts fresh.
func resetToolchainCache() {
	toolchainOnce = sync.Once{}
	cachedToolchainPaths = nil
}

func TestDetectToolchainPaths_EmptyWhenNoneExist(t *testing.T) {
	resetToolchainCache()

	// Clear all known env vars so no env-based paths are found.
	for _, tc := range knownToolchains {
		t.Setenv(tc.envVar, "")
	}
	// Point HOME to an empty temp dir so fallback paths don't exist.
	empty := t.TempDir()
	t.Setenv("HOME", empty)

	paths := detectToolchainPaths()
	if len(paths) != 0 {
		t.Errorf("expected no paths, got %v", paths)
	}
}

func TestDetectToolchainPaths_ResolvesEnvVar(t *testing.T) {
	resetToolchainCache()

	dir := t.TempDir()
	t.Setenv("NVM_DIR", dir)

	// Clear other env vars to isolate this test.
	for _, tc := range knownToolchains {
		if tc.envVar != "NVM_DIR" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", t.TempDir())

	paths := detectToolchainPaths()

	// Resolve dir the same way the function does.
	resolved, _ := filepath.EvalSymlinks(dir)
	found := false
	for _, p := range paths {
		if p == resolved {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected %q in paths, got %v", resolved, paths)
	}
}

func TestDetectToolchainPaths_SkipsNonExistent(t *testing.T) {
	resetToolchainCache()

	// Point env var at a path that doesn't exist.
	t.Setenv("NVM_DIR", "/nonexistent/nvm/path")
	for _, tc := range knownToolchains {
		if tc.envVar != "NVM_DIR" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", t.TempDir())

	paths := detectToolchainPaths()
	for _, p := range paths {
		if p == "/nonexistent/nvm/path" {
			t.Errorf("should not include non-existent path %q", p)
		}
	}
}

func TestDetectToolchainPaths_Deduplicates(t *testing.T) {
	resetToolchainCache()

	// Create a single directory and point two env vars at it.
	dir := t.TempDir()
	t.Setenv("RUSTUP_HOME", dir)
	t.Setenv("CARGO_HOME", dir)

	for _, tc := range knownToolchains {
		if tc.envVar != "RUSTUP_HOME" && tc.envVar != "CARGO_HOME" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", t.TempDir())

	paths := detectToolchainPaths()

	resolved, _ := filepath.EvalSymlinks(dir)
	count := 0
	for _, p := range paths {
		if p == resolved {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected %q exactly once, found %d times in %v", resolved, count, paths)
	}
}

func TestDetectToolchainPaths_FallbackPath(t *testing.T) {
	resetToolchainCache()

	home := t.TempDir()
	// Create the .nvm fallback directory inside the temp home.
	nvmDir := filepath.Join(home, ".nvm")
	if err := os.MkdirAll(nvmDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Clear NVM_DIR so the function falls back to ~/.nvm.
	t.Setenv("NVM_DIR", "")
	for _, tc := range knownToolchains {
		if tc.envVar != "NVM_DIR" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", home)

	paths := detectToolchainPaths()

	resolved, _ := filepath.EvalSymlinks(nvmDir)
	found := false
	for _, p := range paths {
		if p == resolved {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected fallback %q in paths, got %v", resolved, paths)
	}
}

func TestDetectToolchainPaths_Cached(t *testing.T) {
	resetToolchainCache()

	dir := t.TempDir()
	t.Setenv("NVM_DIR", dir)
	for _, tc := range knownToolchains {
		if tc.envVar != "NVM_DIR" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", t.TempDir())

	first := detectToolchainPaths()
	second := detectToolchainPaths()

	if len(first) != len(second) {
		t.Fatalf("cached result differs: %v vs %v", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("cached result differs at index %d: %q vs %q", i, first[i], second[i])
		}
	}
}

func TestAncestorPaths(t *testing.T) {
	tests := []struct {
		name     string
		paths    []string
		expected []string
	}{
		{
			name:     "single deep path",
			paths:    []string{"/a/b/c/d"},
			expected: []string{"/a", "/a/b", "/a/b/c"},
		},
		{
			name:     "excludes allowed paths",
			paths:    []string{"/a/b/c", "/a"},
			expected: []string{"/a/b"},
		},
		{
			name:     "excludes root",
			paths:    []string{"/a"},
			expected: nil,
		},
		{
			name:     "deduplicates shared ancestors",
			paths:    []string{"/a/b/c", "/a/b/d"},
			expected: []string{"/a", "/a/b"},
		},
		{
			name:     "empty input",
			paths:    nil,
			expected: nil,
		},
		{
			name:     "root-only input",
			paths:    []string{"/"},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ancestorPaths(tt.paths)
			sort.Strings(got)
			sort.Strings(tt.expected)
			if len(got) != len(tt.expected) {
				t.Fatalf("ancestorPaths(%v) = %v, want %v", tt.paths, got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("ancestorPaths(%v)[%d] = %q, want %q", tt.paths, i, got[i], tt.expected[i])
				}
			}
		})
	}
}
