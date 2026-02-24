package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultDataDirNonEmpty(t *testing.T) {
	dir := DefaultDataDir()
	if dir == "" {
		t.Fatal("DefaultDataDir returned empty string")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("DefaultDataDir should return absolute path, got %q", dir)
	}
}
