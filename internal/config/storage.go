package config

import (
	"os"
	"path/filepath"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func StorageLayout(td *FabricDef, dataDir string) runtime.StorageLayout {
	layout := runtime.StorageLayout{
		SharedRoot:  filepath.Join(dataDir, "shared"),
		AgentRoot:   filepath.Join(dataDir, "agents"),
		ScratchRoot: os.TempDir(),
	}
	if td == nil {
		return layout
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
	return layout
}
