package config

import (
	"fmt"
	"path/filepath"

	"github.com/razvanmaftei/agentfab/defaults"
)

// InitProject creates a self-contained project directory with default agents.
func InitProject(name, dir string) (string, error) {
	agentsDir := filepath.Join(dir, "defaults", "agents")

	if err := ExtractDefaultAgents(defaults.AgentFS, agentsDir); err != nil {
		return "", fmt.Errorf("extract default agents: %w", err)
	}

	td := &FabricDef{
		Fabric: FabricMeta{
			Name:    name,
			Version: 1,
		},
		AgentsDir: "defaults/agents",
	}
	if err := td.ResolveAgents(); err != nil {
		return "", fmt.Errorf("resolve agents: %w", err)
	}

	configPath := filepath.Join(dir, "agents.yaml")
	if err := WriteFabricDef(configPath, td); err != nil {
		return "", fmt.Errorf("write agents.yaml: %w", err)
	}

	manifest, err := GenerateManifest(agentsDir)
	if err != nil {
		return "", fmt.Errorf("generate manifest: %w", err)
	}
	if err := WriteManifest(ManifestPath(agentsDir), manifest); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	return configPath, nil
}

// InitProjectCustom creates a project with custom agents compiled from .md descriptions.
func InitProjectCustom(name, dir, mdDir, defaultModel string) (string, error) {
	descs, err := ReadAgentDescriptions(mdDir)
	if err != nil {
		return "", fmt.Errorf("read agent descriptions: %w", err)
	}

	results, err := CompileAgents(descs, defaultModel)
	if err != nil {
		return "", fmt.Errorf("compile agents: %w", err)
	}

	agentsDir := filepath.Join(dir, "agents")
	if err := WriteCompiledAgents(agentsDir, results); err != nil {
		return "", fmt.Errorf("write compiled agents: %w", err)
	}

	td := &FabricDef{
		Fabric: FabricMeta{
			Name:    name,
			Version: 1,
		},
		AgentsDir: "agents",
	}
	if err := td.ResolveAgents(); err != nil {
		return "", fmt.Errorf("resolve agents: %w", err)
	}

	configPath := filepath.Join(dir, "agents.yaml")
	if err := WriteFabricDef(configPath, td); err != nil {
		return "", fmt.Errorf("write agents.yaml: %w", err)
	}

	manifest, err := GenerateManifest(agentsDir)
	if err != nil {
		return "", fmt.Errorf("generate manifest: %w", err)
	}
	if err := WriteManifest(ManifestPath(agentsDir), manifest); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	return configPath, nil
}
