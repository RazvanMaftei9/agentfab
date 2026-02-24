package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ProjectEntry represents a registered project in the project registry.
type ProjectEntry struct {
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// LoadProjectRegistry reads the project registry from disk.
// Returns an empty slice (no error) if the file does not exist.
func LoadProjectRegistry() ([]ProjectEntry, error) {
	path := RegistryPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project registry: %w", err)
	}

	var entries []ProjectEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse project registry: %w", err)
	}

	sortProjects(entries)
	return entries, nil
}

// SaveProjectRegistry writes the project registry to disk.
func SaveProjectRegistry(entries []ProjectEntry) error {
	path := RegistryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create registry dir: %w", err)
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal project registry: %w", err)
	}
	data = append(data, '\n')

	return os.WriteFile(path, data, 0644)
}

// AddProject adds a new project entry and saves the registry.
func AddProject(name, dir string) ([]ProjectEntry, error) {
	entries, err := LoadProjectRegistry()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	entries = append(entries, ProjectEntry{
		Name:       name,
		Dir:        dir,
		CreatedAt:  now,
		LastUsedAt: now,
	})

	sortProjects(entries)
	if err := SaveProjectRegistry(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// TouchProject updates LastUsedAt for the named project and saves.
func TouchProject(entries []ProjectEntry, name string) ([]ProjectEntry, error) {
	for i := range entries {
		if entries[i].Name == name {
			entries[i].LastUsedAt = time.Now().UTC()
			break
		}
	}
	sortProjects(entries)
	if err := SaveProjectRegistry(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// RemoveProject removes the named project from the registry and saves (does not delete files on disk).
func RemoveProject(entries []ProjectEntry, name string) ([]ProjectEntry, error) {
	var filtered []ProjectEntry
	for _, e := range entries {
		if e.Name != name {
			filtered = append(filtered, e)
		}
	}

	if err := SaveProjectRegistry(filtered); err != nil {
		return nil, err
	}
	return filtered, nil
}

func sortProjects(entries []ProjectEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastUsedAt.After(entries[j].LastUsedAt)
	})
}
