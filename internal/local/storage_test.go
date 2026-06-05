package local

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func TestStorageReadWrite(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	// Write to agent tier.
	err := s.Write(ctx, runtime.TierAgent, "test.txt", []byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	data, err := s.Read(ctx, runtime.TierAgent, "test.txt")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("got %q, want %q", data, "hello")
	}
}

func TestStorageAppend(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "conductor")
	ctx := context.Background()

	path := "logs/req-1.jsonl"
	s.Append(ctx, runtime.TierShared, path, []byte("line1\n"))
	s.Append(ctx, runtime.TierShared, path, []byte("line2\n"))

	data, err := s.Read(ctx, runtime.TierShared, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "line1\nline2\n" {
		t.Errorf("got %q", data)
	}
}

func TestStorageList(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "architect")
	ctx := context.Background()

	s.Write(ctx, runtime.TierAgent, "decisions/001.md", []byte("a"))
	s.Write(ctx, runtime.TierAgent, "decisions/002.md", []byte("b"))
	s.Write(ctx, runtime.TierAgent, "reviews/001.md", []byte("c"))

	files, err := s.List(ctx, runtime.TierAgent, "decisions/*.md")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestStorageExists(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	exists, _ := s.Exists(ctx, runtime.TierAgent, "nope.txt")
	if exists {
		t.Error("should not exist")
	}

	s.Write(ctx, runtime.TierAgent, "yes.txt", []byte("y"))
	exists, _ = s.Exists(ctx, runtime.TierAgent, "yes.txt")
	if !exists {
		t.Error("should exist")
	}
}

func TestStorageWriteScoping(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	// Can write to own artifacts.
	err := s.Write(ctx, runtime.TierShared, "artifacts/developer/req-1/file.go", []byte("code"))
	if err != nil {
		t.Fatalf("should allow own artifacts: %v", err)
	}

	// Cannot write to another agent's artifacts.
	err = s.Write(ctx, runtime.TierShared, "artifacts/architect/req-1/file.go", []byte("code"))
	if err == nil {
		t.Fatal("should deny writing to other agent's artifacts")
	}

	// Cannot write arbitrary shared files.
	err = s.Write(ctx, runtime.TierShared, "random.txt", []byte("x"))
	if err == nil {
		t.Fatal("should deny arbitrary shared writes")
	}
}

func TestStorageDeleteFileOnScratch(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	s.Write(ctx, runtime.TierScratch, "test.txt", []byte("hello"))
	exists, _ := s.Exists(ctx, runtime.TierScratch, "test.txt")
	if !exists {
		t.Fatal("file should exist before delete")
	}

	err := s.Delete(ctx, runtime.TierScratch, "test.txt")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	exists, _ = s.Exists(ctx, runtime.TierScratch, "test.txt")
	if exists {
		t.Error("file should not exist after delete")
	}
}

func TestStorageDeleteBlockedOnAgent(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	s.Write(ctx, runtime.TierAgent, "test.txt", []byte("hello"))
	err := s.Delete(ctx, runtime.TierAgent, "test.txt")
	if err == nil {
		t.Fatal("delete on agent tier should be blocked")
	}

	// File should still exist.
	exists, _ := s.Exists(ctx, runtime.TierAgent, "test.txt")
	if !exists {
		t.Error("file should still exist after blocked delete")
	}
}

func TestStorageDeleteBlockedOnShared(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	s.Write(ctx, runtime.TierShared, "artifacts/developer/test.txt", []byte("hello"))
	err := s.Delete(ctx, runtime.TierShared, "artifacts/developer/test.txt")
	if err == nil {
		t.Fatal("delete on shared tier should be blocked")
	}

	// File should still exist.
	exists, _ := s.Exists(ctx, runtime.TierShared, "artifacts/developer/test.txt")
	if !exists {
		t.Error("file should still exist after blocked delete")
	}
}

func TestStorageDeleteDirectory(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	s.Write(ctx, runtime.TierScratch, "scripts/run.sh", []byte("#!/bin/sh"))
	s.Write(ctx, runtime.TierScratch, "scripts/build.sh", []byte("#!/bin/sh"))

	// Delete the entire scratch tier by passing empty path.
	err := s.Delete(ctx, runtime.TierScratch, "")
	if err != nil {
		t.Fatalf("delete scratch: %v", err)
	}

	exists, _ := s.Exists(ctx, runtime.TierScratch, "scripts/run.sh")
	if exists {
		t.Error("scratch files should be gone after delete")
	}
}

func TestStoragePathTraversal(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	_, err := s.Read(ctx, runtime.TierAgent, "../../../etc/passwd")
	if err == nil {
		t.Fatal("should reject path traversal")
	}
}

func TestStorageListRejectsTraversalPattern(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	if _, err := s.List(ctx, runtime.TierAgent, "../*"); err == nil {
		t.Fatal("list should reject traversal pattern")
	}
}

func TestStorageScopeBypassViaDotDot(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(dir, "developer")
	ctx := context.Background()

	// A path like "artifacts/developer/../planner/secret.md" starts with the
	// allowed prefix but resolves to "artifacts/planner/secret.md" — another
	// agent's scope. checkWriteScope must reject this.
	err := s.Write(ctx, runtime.TierShared, "artifacts/developer/../planner/secret.md", []byte("pwned"))
	if err == nil {
		t.Fatal("should reject scope bypass via .. within allowed prefix")
	}
}

// TestNewStorageAbsolutizesRelativeBaseDir is a regression test for the
// us-energy-grid 2026-05-25 nested-path bug.
//
// When NewStorage is called with a relative baseDir (e.g. "./.data" — the
// shape `agentfab run --data-dir ./.data` produces), the derived SharedRoot
// and AgentRoot tier paths must be absolute. Otherwise the values exported
// into agent processes via $SHARED_DIR / $AGENT_DIR resolve relative to the
// agent's own CWD (typically the absolute scratch tempdir from os.TempDir()),
// not the agentfab parent process CWD. Python scripts in agents that did
// `Path(os.environ["SHARED_DIR"]) / "artifacts" / "<agent>"` would then write
// to e.g. `/tmp/agentfab-scratch-xxx/.data/shared/artifacts/<agent>/`, and on
// parent-side artifact promotion that whole subtree would land nested inside
// the agent's actual artifacts directory at
// `<repo>/.data/shared/artifacts/<agent>/.data/shared/artifacts/<agent>/...`.
//
// The fix: absolutize baseDir before deriving tier roots, so the env vars
// agents see are always usable absolute paths regardless of the caller's
// shell shape.
func TestNewStorageAbsolutizesRelativeBaseDir(t *testing.T) {
	// Snapshot and restore CWD — the test deliberately runs from a known
	// directory because the absolute resolution of a relative baseDir
	// depends on CWD at NewStorage call time.
	origCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCWD) })

	workdir := t.TempDir()
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	// macOS resolves /tmp to /private/tmp; normalize so prefix comparisons
	// below are stable across symlink-aware and naive joins.
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		resolvedWorkdir = workdir
	}

	cases := []struct {
		name    string
		baseDir string
	}{
		{"dot-slash relative", "./.data"},
		{"bare relative", ".data"},
		{"plain relative", "data"},
		{"nested relative", "var/lib/agentfab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStorage(tc.baseDir, "developer")

			shared := s.TierDir(runtime.TierShared)
			agent := s.TierDir(runtime.TierAgent)

			if !filepath.IsAbs(shared) {
				t.Fatalf("shared tier path not absolute: %q (baseDir=%q)", shared, tc.baseDir)
			}
			if !filepath.IsAbs(agent) {
				t.Fatalf("agent tier path not absolute: %q (baseDir=%q)", agent, tc.baseDir)
			}

			// And the absolute path must resolve under the test workdir,
			// not / or some surprising parent. Normalize through EvalSymlinks
			// on the actual paths to handle the macOS /tmp → /private/tmp case.
			resolvedShared, err := filepath.EvalSymlinks(filepath.Dir(shared))
			if err != nil {
				resolvedShared = filepath.Dir(shared)
			}
			if !strings.HasPrefix(resolvedShared, resolvedWorkdir) {
				t.Fatalf("shared tier %q does not resolve under workdir %q (baseDir=%q)",
					shared, resolvedWorkdir, tc.baseDir)
			}
		})
	}
}

// TestNewStorageAcceptsAbsoluteBaseDir verifies that the absolutize step is
// idempotent: passing an already-absolute baseDir still produces the same
// (absolute) tier paths.
func TestNewStorageAcceptsAbsoluteBaseDir(t *testing.T) {
	base := t.TempDir() // os.MkdirTemp returns an absolute path
	if !filepath.IsAbs(base) {
		t.Fatalf("t.TempDir() returned non-absolute path %q — test premise broken", base)
	}

	s := NewStorage(base, "developer")

	wantShared := filepath.Join(base, "shared")
	wantAgent := filepath.Join(base, "agents", "developer")

	if got := s.TierDir(runtime.TierShared); got != wantShared {
		t.Fatalf("shared tier = %q, want %q", got, wantShared)
	}
	if got := s.TierDir(runtime.TierAgent); got != wantAgent {
		t.Fatalf("agent tier = %q, want %q", got, wantAgent)
	}
}

func TestStorageRespectsConfiguredLayout(t *testing.T) {
	base := t.TempDir()
	layout := runtime.StorageLayout{
		SharedRoot:  base + "/fabric-shared",
		AgentRoot:   base + "/fabric-agents",
		ScratchRoot: base + "/fabric-scratch",
	}
	s := NewStorageWithLayout(layout, "developer")

	if got := s.TierDir(runtime.TierShared); got != layout.SharedRoot {
		t.Fatalf("shared tier = %q, want %q", got, layout.SharedRoot)
	}
	if got := s.TierDir(runtime.TierAgent); got != layout.AgentRoot+"/developer" {
		t.Fatalf("agent tier = %q", got)
	}
	if got := s.TierDir(runtime.TierScratch); got != layout.ScratchRoot+"/agentfab-developer" {
		t.Fatalf("scratch tier = %q", got)
	}
}
