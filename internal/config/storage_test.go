package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStorageLayoutAbsolutizesRelativeDataDir is a regression test for the
// us-energy-grid 2026-05-25 nested-path bug at the actual production call
// site. This function (config.StorageLayout) is what cmd/agentfab/main.go
// uses to build the runtime.StorageLayout that gets passed to
// local.NewStorageWithLayout; local.NewStorage itself is only used by tests.
//
// The bug: when --data-dir is a relative path (e.g. "./.data"), the derived
// SharedRoot/AgentRoot were also relative. Exported into agent processes as
// $SHARED_DIR/$AGENT_DIR, those relative paths resolve against the agent's
// own CWD (the scratch tempdir) rather than the agentfab parent process CWD.
// Python scripts then wrote to /tmp/agentfab-scratch/.data/shared/artifacts/<agent>/...
// and parent-side promotion produced the nested
// <repo>/.data/shared/artifacts/<agent>/.data/shared/artifacts/<agent>/... tree.
func TestStorageLayoutAbsolutizesRelativeDataDir(t *testing.T) {
	origCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCWD) })

	workdir := t.TempDir()
	if err := os.Chdir(workdir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		resolvedWorkdir = workdir
	}

	cases := []struct {
		name    string
		dataDir string
	}{
		{"dot-slash relative", "./.data"},
		{"bare relative", ".data"},
		{"plain relative", "data"},
		{"nested relative", "var/lib/agentfab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			layout := StorageLayout(nil, tc.dataDir)

			if !filepath.IsAbs(layout.SharedRoot) {
				t.Fatalf("SharedRoot not absolute: %q (dataDir=%q)", layout.SharedRoot, tc.dataDir)
			}
			if !filepath.IsAbs(layout.AgentRoot) {
				t.Fatalf("AgentRoot not absolute: %q (dataDir=%q)", layout.AgentRoot, tc.dataDir)
			}
			if !filepath.IsAbs(layout.ScratchRoot) {
				t.Fatalf("ScratchRoot not absolute: %q (dataDir=%q)", layout.ScratchRoot, tc.dataDir)
			}

			// And the absolute paths must resolve under the test workdir.
			// Normalize through EvalSymlinks to handle macOS /tmp → /private/tmp.
			for _, root := range []string{layout.SharedRoot, layout.AgentRoot} {
				resolved, err := filepath.EvalSymlinks(filepath.Dir(root))
				if err != nil {
					resolved = filepath.Dir(root)
				}
				if !strings.HasPrefix(resolved, resolvedWorkdir) {
					t.Fatalf("tier root %q does not resolve under workdir %q (dataDir=%q)",
						root, resolvedWorkdir, tc.dataDir)
				}
			}
		})
	}
}

// TestStorageLayoutAbsolutizesFabricDefOverrides verifies that even when a
// fabric-def supplies its own shared_root / agent_root (potentially relative
// because users write whatever they want in agents.yaml), the result is
// absolutized before reaching agent processes.
func TestStorageLayoutAbsolutizesFabricDefOverrides(t *testing.T) {
	origCWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCWD) })
	workdir := t.TempDir()
	_ = os.Chdir(workdir)

	td := &FabricDef{}
	td.Storage.SharedRoot = "./custom-shared"
	td.Storage.AgentRoot = "custom-agents"
	td.Storage.ScratchRoot = "custom-scratch"

	layout := StorageLayout(td, "./.data") // dataDir ignored when overrides set

	if !filepath.IsAbs(layout.SharedRoot) {
		t.Fatalf("SharedRoot override not absolute: %q", layout.SharedRoot)
	}
	if !filepath.IsAbs(layout.AgentRoot) {
		t.Fatalf("AgentRoot override not absolute: %q", layout.AgentRoot)
	}
	if !filepath.IsAbs(layout.ScratchRoot) {
		t.Fatalf("ScratchRoot override not absolute: %q", layout.ScratchRoot)
	}
}

// TestStorageLayoutAcceptsAbsoluteDataDir confirms idempotence: an
// already-absolute dataDir produces correct, unchanged tier paths.
func TestStorageLayoutAcceptsAbsoluteDataDir(t *testing.T) {
	base := t.TempDir()
	layout := StorageLayout(nil, base)

	wantShared := filepath.Join(base, "shared")
	wantAgent := filepath.Join(base, "agents")

	if layout.SharedRoot != wantShared {
		t.Fatalf("SharedRoot = %q, want %q", layout.SharedRoot, wantShared)
	}
	if layout.AgentRoot != wantAgent {
		t.Fatalf("AgentRoot = %q, want %q", layout.AgentRoot, wantAgent)
	}
}
