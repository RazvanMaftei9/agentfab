package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/runtime"
	"gopkg.in/yaml.v3"
)

// LoadAgentsDir reads all *.yaml files from a directory and returns agent definitions.
// Co-located {name}.md files are auto-set as SpecialKnowledgeFile when empty.
func LoadAgentsDir(dir string) ([]runtime.AgentDefinition, error) {
	return LoadAgentsDirFS(os.DirFS(dir), ".")
}

// LoadAgentsDirFS reads agent definitions from an fs.FS rooted at root.
func LoadAgentsDirFS(fsys fs.FS, root string) ([]runtime.AgentDefinition, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		return nil, fmt.Errorf("read agents dir %q: %w", root, err)
	}

	var defs []runtime.AgentDefinition
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := fs.ReadFile(fsys, filepath.Join(root, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read agent file %q: %w", e.Name(), err)
		}
		var def runtime.AgentDefinition
		if err := yaml.Unmarshal(data, &def); err != nil {
			return nil, fmt.Errorf("parse agent file %q: %w", e.Name(), err)
		}

		// Auto-set SpecialKnowledgeFile if co-located .md exists.
		if def.SpecialKnowledgeFile == "" {
			mdName := strings.TrimSuffix(e.Name(), ".yaml") + ".md"
			if _, err := fs.Stat(fsys, filepath.Join(root, mdName)); err == nil {
				def.SpecialKnowledgeFile = mdName
			}
		}

		defs = append(defs, def)
	}
	return defs, nil
}

// LoadAgentFile loads a single agent definition from a YAML file.
func LoadAgentFile(path string) (runtime.AgentDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runtime.AgentDefinition{}, fmt.Errorf("read agent file: %w", err)
	}
	var def runtime.AgentDefinition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return runtime.AgentDefinition{}, fmt.Errorf("parse agent file: %w", err)
	}

	// Auto-set SpecialKnowledgeFile if co-located .md exists.
	if def.SpecialKnowledgeFile == "" {
		mdPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".md"
		if _, err := os.Stat(mdPath); err == nil {
			def.SpecialKnowledgeFile = filepath.Base(mdPath)
		}
	}
	return def, nil
}

// ExtractDefaultAgents writes embedded default agent files to targetDir, skipping existing files.
func ExtractDefaultAgents(fsys fs.FS, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create agents dir: %w", err)
	}

	entries, err := fs.ReadDir(fsys, "agents")
	if err != nil {
		return fmt.Errorf("read embedded agents: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		target := filepath.Join(targetDir, e.Name())
		if _, err := os.Stat(target); err == nil {
			continue // skip existing
		}
		data, err := fs.ReadFile(fsys, "agents/"+e.Name())
		if err != nil {
			return fmt.Errorf("read embedded %q: %w", e.Name(), err)
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			return fmt.Errorf("write %q: %w", target, err)
		}
	}
	return nil
}
