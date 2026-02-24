package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProjectRegistryRoundTrip(t *testing.T) {
	// Use a temp dir so we don't touch the real registry.
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	entries, err := AddProject("my-app", filepath.Join(tmpDir, "my-app"))
	if err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "my-app" {
		t.Errorf("name = %q, want %q", entries[0].Name, "my-app")
	}
	if entries[0].Dir != filepath.Join(tmpDir, "my-app") {
		t.Errorf("dir = %q, want %q", entries[0].Dir, filepath.Join(tmpDir, "my-app"))
	}
	if entries[0].CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}

	// Reload and verify.
	loaded, err := LoadProjectRegistry()
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 entry after reload, got %d", len(loaded))
	}
	if loaded[0].Name != "my-app" {
		t.Errorf("reloaded name = %q, want %q", loaded[0].Name, "my-app")
	}
}

func TestProjectRegistryOrdering(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Add two projects with a small delay.
	_, err := AddProject("older", filepath.Join(tmpDir, "older"))
	if err != nil {
		t.Fatalf("AddProject older: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, err = AddProject("newer", filepath.Join(tmpDir, "newer"))
	if err != nil {
		t.Fatalf("AddProject newer: %v", err)
	}

	entries, err := LoadProjectRegistry()
	if err != nil {
		t.Fatalf("LoadProjectRegistry: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// Most recently used first.
	if entries[0].Name != "newer" {
		t.Errorf("first entry = %q, want %q", entries[0].Name, "newer")
	}
	if entries[1].Name != "older" {
		t.Errorf("second entry = %q, want %q", entries[1].Name, "older")
	}
}

func TestProjectTouch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, _ = AddProject("alpha", filepath.Join(tmpDir, "alpha"))
	time.Sleep(10 * time.Millisecond)
	_, _ = AddProject("beta", filepath.Join(tmpDir, "beta"))

	// Beta is most recent. Touch alpha to make it most recent.
	entries, _ := LoadProjectRegistry()
	time.Sleep(10 * time.Millisecond)
	entries, err := TouchProject(entries, "alpha")
	if err != nil {
		t.Fatalf("TouchProject: %v", err)
	}

	if entries[0].Name != "alpha" {
		t.Errorf("after touch, first entry = %q, want %q", entries[0].Name, "alpha")
	}
}

func TestProjectRemove(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	_, _ = AddProject("keep", filepath.Join(tmpDir, "keep"))
	_, _ = AddProject("remove-me", filepath.Join(tmpDir, "remove-me"))

	entries, _ := LoadProjectRegistry()
	entries, err := RemoveProject(entries, "remove-me")
	if err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry after remove, got %d", len(entries))
	}
	if entries[0].Name != "keep" {
		t.Errorf("remaining entry = %q, want %q", entries[0].Name, "keep")
	}
}

func TestLoadMissingRegistry(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	entries, err := LoadProjectRegistry()
	if err != nil {
		t.Fatalf("expected no error for missing file, got: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(entries))
	}
}
