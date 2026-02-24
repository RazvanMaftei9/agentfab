package sandbox

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// toolchainSpec maps an environment variable to a fallback path relative to $HOME.
// If envVar is set, its value is used; otherwise fallback is tried (empty = skip).
type toolchainSpec struct {
	envVar   string
	fallback string // relative to home, e.g. ".nvm"
}

var knownToolchains = []toolchainSpec{
	// Node.js version managers
	{envVar: "NVM_DIR", fallback: ".nvm"},
	{envVar: "FNM_MULTISHELL_PATH", fallback: ".fnm"},
	{envVar: "VOLTA_HOME", fallback: ".volta"},
	{envVar: "N_PREFIX"},

	// JavaScript runtimes
	{envVar: "BUN_INSTALL", fallback: ".bun"},
	{envVar: "DENO_DIR", fallback: ".deno"},

	// Python
	{envVar: "PYENV_ROOT", fallback: ".pyenv"},
	{envVar: "CONDA_PREFIX"},

	// Ruby
	{envVar: "RBENV_ROOT", fallback: ".rbenv"},
	{envVar: "rvm_path", fallback: ".rvm"},

	// Rust
	{envVar: "RUSTUP_HOME", fallback: ".rustup"},
	{envVar: "CARGO_HOME", fallback: ".cargo"},

	// Java
	{envVar: "SDKMAN_DIR", fallback: ".sdkman"},
	{envVar: "JAVA_HOME"},

	// Go
	{envVar: "GOROOT"},
	{envVar: "GOPATH", fallback: "go"},

	// Mobile
	{envVar: "ANDROID_HOME"},
	{envVar: "FLUTTER_ROOT"},
}

// miscFallbacks are well-known directories (relative to $HOME) that don't have
// a corresponding environment variable.
var miscFallbacks = []string{
	".local/bin",
}

var (
	cachedToolchainPaths []string
	toolchainOnce        sync.Once
)

// detectToolchainPaths returns existing toolchain directories on the host.
// Results are cached for the lifetime of the process.
func detectToolchainPaths() []string {
	toolchainOnce.Do(func() {
		cachedToolchainPaths = probeToolchainPaths()
	})
	return cachedToolchainPaths
}

// probeToolchainPaths does the actual detection work.
func probeToolchainPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	var paths []string

	add := func(p string) {
		if p == "" {
			return
		}
		// Resolve symlinks so sandbox rules match the canonical path.
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	for _, tc := range knownToolchains {
		if v := os.Getenv(tc.envVar); v != "" {
			add(v)
			continue
		}
		if tc.fallback != "" {
			add(filepath.Join(home, tc.fallback))
		}
	}

	for _, rel := range miscFallbacks {
		add(filepath.Join(home, rel))
	}

	return paths
}

// ancestorPaths returns directories that are ancestors of allowedPaths but
// are not themselves in the allowed set. These need file-read-metadata
// (stat-only) access so that tools walking up the directory tree don't
// get EPERM. The root "/" is excluded (already covered in the profile).
func ancestorPaths(allowedPaths []string) []string {
	allowed := make(map[string]struct{}, len(allowedPaths))
	for _, p := range allowedPaths {
		allowed[p] = struct{}{}
	}

	ancestors := make(map[string]struct{})
	for _, p := range allowedPaths {
		for {
			parent := filepath.Dir(p)
			if parent == p || parent == "/" {
				break
			}
			p = parent
			if _, ok := allowed[parent]; ok {
				break
			}
			ancestors[parent] = struct{}{}
		}
	}

	result := make([]string, 0, len(ancestors))
	for a := range ancestors {
		result = append(result, a)
	}
	sort.Strings(result)
	return result
}
