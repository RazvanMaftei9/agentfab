//go:build linux

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

func TestLandlockAvailable(t *testing.T) {
	// On kernel >= 5.13 this should return true.
	// On older kernels the test is skipped rather than failed.
	if !landlockAvailable() {
		t.Skip("Landlock not available on this kernel (requires >= 5.13)")
	}
	t.Log("Landlock is available")
}

func TestEnforceLandlockBasic(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	// EnforceLandlock restricts the calling process, so we can't call it
	// directly in the test process (it would break subsequent tests).
	// Instead we verify it via a child process using sandbox.Run.
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
	testFile := filepath.Join(allowed, "ok.txt")
	output, _, err := Run(context.Background(), cfg, nil,
		"sh", "-c", fmt.Sprintf("echo hello > %s && cat %s", testFile, testFile))
	if err != nil {
		t.Fatalf("write to allowed dir failed: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "hello") {
		t.Errorf("expected 'hello', got: %s", output)
	}

	// Writing outside allowed dir should fail.
	blockedFile := filepath.Join(forbidden, "blocked.txt")
	output, _, err = Run(context.Background(), cfg, nil,
		"sh", "-c", fmt.Sprintf("echo evil > %s 2>&1; echo exit=$?", blockedFile))
	if _, statErr := os.Stat(blockedFile); statErr == nil {
		t.Errorf("Landlock should have prevented writing to %s\noutput: %s", blockedFile, output)
	}
	_ = err
}

func TestEnforceLandlockReadOnly(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	roDir := t.TempDir()
	workDir := t.TempDir()

	// Pre-create a file in the read-only directory.
	roFile := filepath.Join(roDir, "data.txt")
	if err := os.WriteFile(roFile, []byte("read-me"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		WorkDir: workDir,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
			ReadOnly:  []string{roDir},
		},
	}

	// Reading should succeed.
	output, _, err := Run(context.Background(), cfg, nil, "cat", roFile)
	if err != nil {
		t.Fatalf("read from RO dir failed: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "read-me") {
		t.Errorf("expected 'read-me', got: %s", output)
	}

	// Writing to the read-only dir should fail.
	writeFile := filepath.Join(roDir, "nope.txt")
	output, _, err = Run(context.Background(), cfg, nil,
		"sh", "-c", fmt.Sprintf("echo nope > %s 2>&1; echo exit=$?", writeFile))
	if _, statErr := os.Stat(writeFile); statErr == nil {
		t.Errorf("Landlock should have prevented writing to read-only dir: %s", writeFile)
	}
	_ = err
}

func TestRestrictLinuxPassthroughWhenNil(t *testing.T) {
	cfg := Config{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
		// Restrict is nil — no Landlock.
	}

	output, _, err := Run(context.Background(), cfg, nil, "echo", "no-sandbox")
	if err != nil {
		t.Fatalf("run without sandbox: %v", err)
	}
	if !strings.Contains(string(output), "no-sandbox") {
		t.Errorf("expected 'no-sandbox', got: %s", output)
	}
}

func TestRestrictLinuxSystemPathsAccessible(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	workDir := t.TempDir()
	cfg := Config{
		WorkDir: workDir,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
		},
	}

	// System tools should remain accessible (/usr/bin, /bin).
	output, _, err := Run(context.Background(), cfg, nil, "which", "ls")
	if err != nil {
		t.Fatalf("system tools inaccessible under Landlock: %v\noutput: %s", err, output)
	}

	// git should work (installed in Docker image).
	output, _, err = Run(context.Background(), cfg, nil, "git", "--version")
	if err != nil {
		t.Fatalf("git inaccessible under Landlock: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "git version") {
		t.Errorf("unexpected git output: %s", output)
	}
}

func TestRestrictLinuxNpmAccessible(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	workDir := t.TempDir()
	cfg := Config{
		WorkDir: workDir,
		Timeout: 30 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
		},
	}

	// npm should be accessible under the sandbox.
	output, _, err := Run(context.Background(), cfg, nil, "npm", "--version")
	if err != nil {
		t.Fatalf("npm inaccessible under Landlock: %v\noutput: %s", err, output)
	}
	t.Logf("npm version: %s", strings.TrimSpace(string(output)))
}

func TestRestrictLinuxNodeAccessible(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	workDir := t.TempDir()
	cfg := Config{
		WorkDir: workDir,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
		},
	}

	output, _, err := Run(context.Background(), cfg, nil,
		"node", "-e", "console.log('node-ok')")
	if err != nil {
		t.Fatalf("node inaccessible under Landlock: %v\noutput: %s", err, output)
	}
	if !strings.Contains(string(output), "node-ok") {
		t.Errorf("expected 'node-ok', got: %s", output)
	}
}

func TestEnforceLandlockEnvStripping(t *testing.T) {
	if !landlockAvailable() {
		t.Skip("Landlock not available")
	}

	workDir := t.TempDir()
	cfg := Config{
		WorkDir: workDir,
		Timeout: 10 * time.Second,
		Restrict: &Policy{
			ReadWrite: []string{workDir},
		},
	}

	output, _, err := Run(context.Background(), cfg, nil, "env")
	if err != nil {
		t.Fatalf("env failed: %v", err)
	}

	env := string(output)
	if !strings.Contains(env, "PATH=") {
		t.Error("PATH should be present")
	}
	if strings.Contains(env, "ANTHROPIC_API_KEY") {
		t.Error("secrets should not leak into sandboxed env")
	}
}
