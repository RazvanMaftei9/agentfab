//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestrictBlocksWriteOutsidePolicy(t *testing.T) {
	// Create allowed and forbidden directories.
	allowed := t.TempDir()
	forbidden := t.TempDir()

	cfg := Config{
		WorkDir: allowed,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{allowed},
		},
	}

	// Writing inside allowed dir should succeed.
	output, _, err := Run(context.Background(), cfg, nil,
		"sh", "-c", "echo hello > "+filepath.Join(allowed, "test.txt")+" && cat "+filepath.Join(allowed, "test.txt"))
	if err != nil {
		t.Fatalf("write to allowed dir failed: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "hello") {
		t.Errorf("expected 'hello' in output, got: %s", output)
	}

	// Writing outside allowed dir should fail under sandbox-exec.
	forbiddenFile := filepath.Join(forbidden, "blocked.txt")
	output, _, err = Run(context.Background(), cfg, nil,
		"sh", "-c", "echo evil > "+forbiddenFile+" 2>&1; echo exit_code=$?")
	// The command itself may not fail (shell catches the error), but the file
	// should not exist.
	if _, statErr := os.Stat(forbiddenFile); statErr == nil {
		t.Errorf("sandbox should have prevented writing to %s", forbiddenFile)
	}
	_ = output
	_ = err
}

func TestRestrictAllowsReadOnly(t *testing.T) {
	readOnlyDir := t.TempDir()
	workDir := t.TempDir()

	// Create a file in the read-only directory.
	testFile := filepath.Join(readOnlyDir, "data.txt")
	if err := os.WriteFile(testFile, []byte("read-me"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		WorkDir: workDir,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
			ReadOnly:  []string{readOnlyDir},
		},
	}

	// Reading from read-only dir should succeed.
	output, _, err := Run(context.Background(), cfg, nil, "cat", testFile)
	if err != nil {
		t.Fatalf("read from read-only dir failed: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "read-me") {
		t.Errorf("expected 'read-me' in output, got: %s", output)
	}
}

func TestRestrictPassthroughWhenNil(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
		// Restrict is nil — no OS-level sandbox.
	}

	output, _, err := Run(context.Background(), cfg, nil, "echo", "no-sandbox")
	if err != nil {
		t.Fatalf("run without sandbox: %v", err)
	}
	if !strings.Contains(string(output), "no-sandbox") {
		t.Errorf("expected 'no-sandbox' in output, got: %s", output)
	}
}

func TestBuildSBPLIncludesToolchainPaths(t *testing.T) {
	resetToolchainCache()

	// Create a temp dir to act as a toolchain path.
	tcDir := t.TempDir()
	t.Setenv("NVM_DIR", tcDir)

	// Clear other env vars so only NVM_DIR is detected.
	for _, tc := range knownToolchains {
		if tc.envVar != "NVM_DIR" {
			t.Setenv(tc.envVar, "")
		}
	}
	t.Setenv("HOME", t.TempDir())

	cfg := Config{WorkDir: "/tmp/work"}
	policy := Policy{}
	profile := buildSBPL(cfg, policy)

	// Resolve the temp dir the same way detectToolchainPaths does.
	resolved, _ := filepath.EvalSymlinks(tcDir)

	if !strings.Contains(profile, "; Detected toolchain paths (read-only)") {
		t.Error("SBPL profile missing toolchain section header")
	}
	if !strings.Contains(profile, fmt.Sprintf("(subpath %q)", resolved)) {
		t.Errorf("SBPL profile missing toolchain subpath for %q\nprofile:\n%s", resolved, profile)
	}
}

func TestBuildSBPLProfile(t *testing.T) {
	cfg := Config{
		WorkDir: "/tmp/work",
	}
	policy := Policy{
		ReadWrite: []string{"/tmp/scratch", "/tmp/agent"},
		ReadOnly:  []string{"/tmp/shared"},
	}

	profile := buildSBPL(cfg, policy)

	// Verify profile contains expected elements.
	checks := []string{
		"(version 1)",
		"(deny default)",
		"(allow process-exec process-fork)",
		`(subpath "/tmp/scratch")`,
		`(subpath "/tmp/agent")`,
		`(subpath "/tmp/shared")`,
		`(subpath "/tmp/work")`,
		"(allow network*)",
	}
	for _, check := range checks {
		if !strings.Contains(profile, check) {
			t.Errorf("SBPL profile missing %q", check)
		}
	}
}

func TestBuildSBPLIncludesAncestorMetadata(t *testing.T) {
	resetToolchainCache()

	// Clear all toolchain env vars to keep output predictable.
	for _, tc := range knownToolchains {
		t.Setenv(tc.envVar, "")
	}
	t.Setenv("HOME", t.TempDir())

	cfg := Config{
		WorkDir: "/Users/dev/projects/myapp/agents/developer",
	}
	policy := Policy{
		ReadWrite: []string{"/Users/dev/projects/myapp/agents/developer"},
	}

	profile := buildSBPL(cfg, policy)

	// The profile should contain the ancestor metadata section.
	if !strings.Contains(profile, "; Ancestor directories (stat-only for module resolution)") {
		t.Fatal("SBPL profile missing ancestor metadata section")
	}
	if !strings.Contains(profile, "file-read-metadata") {
		t.Fatal("SBPL profile missing file-read-metadata rule")
	}

	// Check that intermediate ancestors of the deeply nested workdir are present.
	expectedAncestors := []string{
		"/Users",
		"/Users/dev",
		"/Users/dev/projects",
		"/Users/dev/projects/myapp",
		"/Users/dev/projects/myapp/agents",
	}
	for _, a := range expectedAncestors {
		literal := fmt.Sprintf("(literal %q)", a)
		if !strings.Contains(profile, literal) {
			t.Errorf("SBPL profile missing ancestor literal for %q\nprofile:\n%s", a, profile)
		}
	}

	// "/" should NOT appear as a literal in the ancestor section (already covered).
	// Count occurrences of (literal "/") — should be exactly 1 (from the system read section).
	count := strings.Count(profile, `(literal "/")`)
	if count != 1 {
		t.Errorf("expected exactly 1 (literal \"/\") entry, found %d", count)
	}
}
