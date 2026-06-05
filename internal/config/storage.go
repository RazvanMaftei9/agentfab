package config

import (
	"os"
	"path/filepath"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func StorageLayout(td *FabricDef, dataDir string) runtime.StorageLayout {
	// Absolutize dataDir before deriving tier roots. If the caller passes a
	// relative dataDir (e.g. `agentfab run --data-dir ./.data`), the resulting
	// SharedRoot/AgentRoot would be relative too — and when exported into agent
	// processes via $SHARED_DIR/$AGENT_DIR, those agents resolve the relative
	// path against their own CWD (the absolute scratch tempdir from os.TempDir()).
	// Python scripts in agents that did
	//   Path(os.environ["SHARED_DIR"]) / "artifacts" / "<agent>"
	// would then write to /tmp/agentfab-scratch-XXX/.data/shared/artifacts/<agent>/
	// and on parent-side promotion that whole subtree would land nested inside
	// the agent's actual artifacts directory at
	//   <repo>/.data/shared/artifacts/<agent>/.data/shared/artifacts/<agent>/...
	// See us-energy-grid dryrun 2026-05-25 nested-path bug for the worked
	// example. The internal/local/storage_test.go regression test guards the
	// downstream code path; this is the upstream half of the same fix.
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	layout := runtime.StorageLayout{
		SharedRoot:  filepath.Join(dataDir, "shared"),
		AgentRoot:   filepath.Join(dataDir, "agents"),
		ScratchRoot: os.TempDir(),
	}
	if td == nil {
		return absolutizeLayout(layout)
	}
	if td.Storage.SharedRoot != "" {
		layout.SharedRoot = td.Storage.SharedRoot
	}
	if td.Storage.AgentRoot != "" {
		layout.AgentRoot = td.Storage.AgentRoot
	}
	if td.Storage.ScratchRoot != "" {
		layout.ScratchRoot = td.Storage.ScratchRoot
	}
	// If any of the fabric-def overrides are relative (e.g. a user-provided
	// agents.yaml with shared_root: ./blob), absolutize each one independently.
	return absolutizeLayout(layout)
}

// absolutizeLayout converts any relative tier root to an absolute path. Returns
// the layout unchanged if every field is already absolute.
func absolutizeLayout(layout runtime.StorageLayout) runtime.StorageLayout {
	if abs, err := filepath.Abs(layout.SharedRoot); err == nil {
		layout.SharedRoot = abs
	}
	if abs, err := filepath.Abs(layout.AgentRoot); err == nil {
		layout.AgentRoot = abs
	}
	if abs, err := filepath.Abs(layout.ScratchRoot); err == nil {
		layout.ScratchRoot = abs
	}
	return layout
}
