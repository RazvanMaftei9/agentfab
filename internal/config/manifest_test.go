package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Create test agent files.
	writeTestFile(t, dir, "agent1.yaml", "name: agent1\nmodel: gpt-4\n")
	writeTestFile(t, dir, "agent1.md", "# Agent 1 knowledge\n")
	writeTestFile(t, dir, "agent2.yaml", "name: agent2\nmodel: claude\n")

	// Generate and write manifest.
	m, err := GenerateManifest(dir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	if len(m.Checksums) != 3 {
		t.Fatalf("expected 3 checksums, got %d", len(m.Checksums))
	}

	mPath := ManifestPath(dir)
	if err := WriteManifest(mPath, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	// Load and verify — should pass.
	loaded, err := LoadManifest(mPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	ok, mismatches, err := VerifyManifest(dir, loaded)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if !ok {
		t.Errorf("expected verification to pass, got mismatches: %v", mismatches)
	}
}

func TestManifestTamperedFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "agent.yaml", "name: agent\nmodel: gpt-4\n")

	m, err := GenerateManifest(dir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}

	// Tamper with the file.
	writeTestFile(t, dir, "agent.yaml", "name: agent\nmodel: gpt-4\nhacked: true\n")

	ok, mismatches, err := VerifyManifest(dir, m)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if ok {
		t.Fatal("expected verification to fail for tampered file")
	}
	if len(mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
	}
	if mismatches[0] != "modified: agent.yaml" {
		t.Errorf("unexpected mismatch: %s", mismatches[0])
	}
}

func TestManifestAddedFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "agent.yaml", "name: agent\n")

	m, err := GenerateManifest(dir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}

	// Add a new file after manifest was generated.
	writeTestFile(t, dir, "new_agent.yaml", "name: new_agent\n")

	ok, mismatches, err := VerifyManifest(dir, m)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if ok {
		t.Fatal("expected verification to fail for added file")
	}
	if len(mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
	}
	if mismatches[0] != "added: new_agent.yaml" {
		t.Errorf("unexpected mismatch: %s", mismatches[0])
	}
}

func TestManifestMissingFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "agent.yaml", "name: agent\n")
	writeTestFile(t, dir, "agent.md", "# Knowledge\n")

	m, err := GenerateManifest(dir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}

	// Remove a file.
	os.Remove(filepath.Join(dir, "agent.md"))

	ok, mismatches, err := VerifyManifest(dir, m)
	if err != nil {
		t.Fatalf("VerifyManifest: %v", err)
	}
	if ok {
		t.Fatal("expected verification to fail for missing file")
	}
	if len(mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
	}
	if mismatches[0] != "missing: agent.md" {
		t.Errorf("unexpected mismatch: %s", mismatches[0])
	}
}

func TestManifestIgnoresNonAgentFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "agent.yaml", "name: agent\n")
	writeTestFile(t, dir, "notes.txt", "some notes\n")
	writeTestFile(t, dir, "data.json", "{}\n")

	m, err := GenerateManifest(dir)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	if len(m.Checksums) != 1 {
		t.Errorf("expected 1 checksum (yaml only), got %d: %v", len(m.Checksums), m.Checksums)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
