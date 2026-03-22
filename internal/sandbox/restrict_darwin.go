//go:build darwin

package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	darwinWarnOnce sync.Once
)

// restrict wraps a command with macOS sandbox-exec using an SBPL profile
// that restricts filesystem access to the paths specified in the policy.
//
// sandbox-exec is deprecated by Apple but remains functional. A warning is
// logged on first use. See TD-006 in docs/tech-debt.md.
func restrict(cfg Config, policy Policy, name string, args []string) (string, []string) {
	darwinWarnOnce.Do(func() {
		slog.Warn("using macOS sandbox-exec (deprecated by Apple, see TD-006)")
	})

	profile := buildSBPL(cfg, policy)

	// Write the profile to a temp file.
	tmpDir := os.TempDir()
	f, err := os.CreateTemp(tmpDir, "agentfab-sandbox-*.sb")
	if err != nil {
		slog.Error("failed to create sandbox profile", "error", err)
		return name, args
	}
	if _, err := f.WriteString(profile); err != nil {
		f.Close()
		os.Remove(f.Name())
		slog.Error("failed to write sandbox profile", "error", err)
		return name, args
	}
	profilePath := f.Name()
	f.Close()

	// Build wrapped command: sandbox-exec -f <profile> <original command>
	newArgs := make([]string, 0, len(args)+3)
	newArgs = append(newArgs, "-f", profilePath, name)
	newArgs = append(newArgs, args...)
	return "sandbox-exec", newArgs
}

// buildSBPL generates a Scheme-based PowerBox Language (SBPL) profile for macOS sandbox-exec.
func buildSBPL(cfg Config, policy Policy) string {
	var sb strings.Builder

	sb.WriteString("(version 1)\n")
	sb.WriteString("(deny default)\n\n")

	// Allow process execution and forking (required for shell commands).
	sb.WriteString("(allow process-exec process-fork)\n")
	sb.WriteString("(allow process*)\n\n")

	// Allow basic system operations.
	sb.WriteString("(allow sysctl-read)\n")
	sb.WriteString("(allow mach-lookup)\n")
	sb.WriteString("(allow signal)\n")
	sb.WriteString("(allow ipc-posix-shm-read-data)\n\n")

	// Allow reading system libraries, frameworks, and executables.
	sb.WriteString("; System read access\n")
	sb.WriteString("(allow file-read*\n")
	sb.WriteString("  (subpath \"/usr\")\n")
	sb.WriteString("  (subpath \"/bin\")\n")
	sb.WriteString("  (subpath \"/sbin\")\n")
	sb.WriteString("  (subpath \"/Library\")\n")
	sb.WriteString("  (subpath \"/System\")\n")
	sb.WriteString("  (subpath \"/Applications\")\n")
	sb.WriteString("  (subpath \"/private/var\")\n")
	sb.WriteString("  (subpath \"/private/etc\")\n")
	sb.WriteString("  (subpath \"/dev\")\n")
	sb.WriteString("  (subpath \"/var\")\n")
	sb.WriteString("  (subpath \"/etc\")\n")
	sb.WriteString("  (subpath \"/private/tmp\")\n")
	sb.WriteString("  (literal \"/\")\n")
	sb.WriteString("  (literal \"/dev/null\")\n")
	sb.WriteString(")\n\n")

	sb.WriteString("; Explicit write access to /dev/null and /dev/zero\n")
	sb.WriteString("(allow file-write*\n")
	sb.WriteString("  (literal \"/dev/null\")\n")
	sb.WriteString("  (literal \"/dev/zero\")\n")
	sb.WriteString(")\n\n")

	// Allow writes only to the dedicated TMPDIR injected by sandbox.Run.
	sb.WriteString("; Temp directory access\n")
	sb.WriteString("(allow file-read* file-write*\n")
	for _, p := range absPaths(sandboxTempDir()) {
		sb.WriteString(fmt.Sprintf("  (subpath %q)\n", p))
	}
	sb.WriteString(")\n\n")

	// Allow Homebrew paths (commonly needed for developer tools).
	sb.WriteString("; Homebrew and developer tools\n")
	sb.WriteString("(allow file-read*\n")
	sb.WriteString("  (subpath \"/opt/homebrew\")\n")
	sb.WriteString("  (subpath \"/usr/local\")\n")
	sb.WriteString(")\n\n")

	// Detected toolchain paths (read-only).
	if tcPaths := detectToolchainPaths(); len(tcPaths) > 0 {
		sb.WriteString("; Detected toolchain paths (read-only)\n")
		sb.WriteString("(allow file-read*\n")
		for _, p := range tcPaths {
			sb.WriteString(fmt.Sprintf("  (subpath %q)\n", p))
		}
		sb.WriteString(")\n\n")
	}

	// Read-write paths from the policy.
	if len(policy.ReadWrite) > 0 {
		sb.WriteString("; Agent read-write paths\n")
		sb.WriteString("(allow file-read* file-write*\n")
		for _, p := range policy.ReadWrite {
			for _, ap := range absPaths(p) {
				sb.WriteString(fmt.Sprintf("  (subpath %q)\n", ap))
			}
		}
		sb.WriteString(")\n\n")
	}

	// Read-only paths from the policy.
	if len(policy.ReadOnly) > 0 {
		sb.WriteString("; Agent read-only paths\n")
		sb.WriteString("(allow file-read*\n")
		for _, p := range policy.ReadOnly {
			for _, ap := range absPaths(p) {
				sb.WriteString(fmt.Sprintf("  (subpath %q)\n", ap))
			}
		}
		sb.WriteString(")\n\n")
	}

	// WorkDir always gets read-write access.
	if cfg.WorkDir != "" {
		sb.WriteString("; Work directory\n")
		sb.WriteString("(allow file-read* file-write*\n")
		for _, ap := range absPaths(cfg.WorkDir) {
			sb.WriteString(fmt.Sprintf("  (subpath %q)\n", ap))
		}
		sb.WriteString(")\n\n")
	}

	// Ancestor directories need stat-only access so tools that walk up
	// the directory tree (e.g. Node module resolution) don't hit EPERM.
	var subpathPaths []string
	// System paths.
	subpathPaths = append(subpathPaths, "/usr", "/bin", "/sbin", "/Library", "/System",
		"/Applications", "/private/var", "/private/etc", "/dev", "/var", "/etc", "/private/tmp")
	// Homebrew.
	subpathPaths = append(subpathPaths, "/opt/homebrew", "/usr/local")
	// Toolchains.
	subpathPaths = append(subpathPaths, detectToolchainPaths()...)
	// Policy RW.
	for _, p := range policy.ReadWrite {
		subpathPaths = append(subpathPaths, absPaths(p)...)
	}
	// Policy RO.
	for _, p := range policy.ReadOnly {
		subpathPaths = append(subpathPaths, absPaths(p)...)
	}
	// WorkDir.
	if cfg.WorkDir != "" {
		subpathPaths = append(subpathPaths, absPaths(cfg.WorkDir)...)
	}
	// Dedicated temp dir.
	subpathPaths = append(subpathPaths, absPaths(sandboxTempDir())...)

	if ancestors := ancestorPaths(subpathPaths); len(ancestors) > 0 {
		sb.WriteString("; Ancestor directories (stat-only for module resolution)\n")
		sb.WriteString("(allow file-read-metadata\n")
		for _, a := range ancestors {
			sb.WriteString(fmt.Sprintf("  (literal %q)\n", a))
		}
		sb.WriteString(")\n\n")
	}

	// Allow network access (agents may need to fetch packages, call APIs).
	sb.WriteString("(allow network*)\n")

	return sb.String()
}

func absPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	// Resolve symlinks — critical on macOS where /var → /private/var.
	// sandbox-exec uses the canonical path for matching.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// absPaths returns both the resolved and unresolved absolute paths for a path.
// On macOS, symlinks like /tmp → /private/tmp and /var → /private/var mean
// a process might reference either form. sandbox-exec matches against the
// canonical path, but some tools construct paths using the unresolved form.
// Emitting both ensures the SBPL policy covers both access patterns.
func absPaths(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return []string{p}
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return []string{abs}
	}
	if resolved == abs {
		return []string{abs}
	}
	return []string{resolved, abs}
}
