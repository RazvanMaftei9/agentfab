package config

import (
	"fmt"
	"path/filepath"
)

func ResolvePathsRelativeToConfig(td *FabricDef, configPath string) error {
	if td == nil || configPath == "" {
		return nil
	}

	configDir := filepath.Dir(configPath)
	if td.AgentsDir != "" && !filepath.IsAbs(td.AgentsDir) {
		td.AgentsDir = filepath.Join(configDir, td.AgentsDir)
		td.Agents = nil
		if err := td.ResolveAgents(); err != nil {
			return fmt.Errorf("resolve agents from config path: %w", err)
		}
	}

	for i, path := range td.Security.TrustedBundlePublicKeys {
		if path == "" || filepath.IsAbs(path) {
			continue
		}
		td.Security.TrustedBundlePublicKeys[i] = filepath.Join(configDir, path)
	}

	if td.Storage.SharedRoot != "" && !filepath.IsAbs(td.Storage.SharedRoot) {
		td.Storage.SharedRoot = filepath.Join(configDir, td.Storage.SharedRoot)
	}
	if td.Storage.AgentRoot != "" && !filepath.IsAbs(td.Storage.AgentRoot) {
		td.Storage.AgentRoot = filepath.Join(configDir, td.Storage.AgentRoot)
	}
	if td.Storage.ScratchRoot != "" && !filepath.IsAbs(td.Storage.ScratchRoot) {
		td.Storage.ScratchRoot = filepath.Join(configDir, td.Storage.ScratchRoot)
	}
	if td.Identity.Mounted.CertFile != "" && !filepath.IsAbs(td.Identity.Mounted.CertFile) {
		td.Identity.Mounted.CertFile = filepath.Join(configDir, td.Identity.Mounted.CertFile)
	}
	if td.Identity.Mounted.KeyFile != "" && !filepath.IsAbs(td.Identity.Mounted.KeyFile) {
		td.Identity.Mounted.KeyFile = filepath.Join(configDir, td.Identity.Mounted.KeyFile)
	}
	if td.Identity.Mounted.BundleFile != "" && !filepath.IsAbs(td.Identity.Mounted.BundleFile) {
		td.Identity.Mounted.BundleFile = filepath.Join(configDir, td.Identity.Mounted.BundleFile)
	}
	return nil
}
