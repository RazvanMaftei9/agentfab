package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Manifest stores SHA-256 checksums for agent definition files to detect unauthorized modifications.
type Manifest struct {
	Version     int               `json:"version"`
	GeneratedAt string            `json:"generated_at"`
	Checksums   map[string]string `json:"checksums"` // relative path → SHA-256 hex
}

var manifestExtensions = map[string]bool{
	".yaml": true,
	".md":   true,
}

// GenerateManifest walks agentsDir and hashes every .yaml and .md file.
func GenerateManifest(agentsDir string) (*Manifest, error) {
	checksums := make(map[string]string)

	err := filepath.Walk(agentsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !manifestExtensions[ext] {
			return nil
		}

		rel, err := filepath.Rel(agentsDir, path)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", path, err)
		}
		rel = filepath.ToSlash(rel) // normalize for cross-platform consistency

		hash, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("hash %q: %w", rel, err)
		}
		checksums[rel] = hash
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk agents dir: %w", err)
	}

	return &Manifest{
		Version:     1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Checksums:   checksums,
	}, nil
}

// WriteManifest writes a manifest as JSON to the given path.
func WriteManifest(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

// LoadManifest reads and parses a manifest JSON file.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// ManifestPath returns the default manifest file path for an agents directory.
func ManifestPath(agentsDir string) string {
	return filepath.Join(agentsDir, "manifest.json")
}

// VerifyManifest re-hashes files in agentsDir and compares against stored checksums.
// Mismatches contains human-readable descriptions of differences.
func VerifyManifest(agentsDir string, m *Manifest) (ok bool, mismatches []string, err error) {
	current, err := GenerateManifest(agentsDir)
	if err != nil {
		return false, nil, fmt.Errorf("generate current checksums: %w", err)
	}

	storedPaths := sortedKeys(m.Checksums)
	for _, rel := range storedPaths {
		expectedHash := m.Checksums[rel]
		actualHash, exists := current.Checksums[rel]
		if !exists {
			mismatches = append(mismatches, fmt.Sprintf("missing: %s", rel))
		} else if actualHash != expectedHash {
			mismatches = append(mismatches, fmt.Sprintf("modified: %s", rel))
		}
	}

	currentPaths := sortedKeys(current.Checksums)
	for _, rel := range currentPaths {
		if _, exists := m.Checksums[rel]; !exists {
			mismatches = append(mismatches, fmt.Sprintf("added: %s", rel))
		}
	}

	return len(mismatches) == 0, mismatches, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
