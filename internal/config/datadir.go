package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultDataDir returns the platform-appropriate default data directory.
func DefaultDataDir() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Documents", "agentfab-data")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "agentfab", "data")
		}
		return filepath.Join(homeDir(), "AppData", "Roaming", "agentfab", "data")
	default: // linux and others
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "agentfab")
		}
		return filepath.Join(homeDir(), ".local", "share", "agentfab")
	}
}

// DefaultProjectsBase returns the platform-appropriate base directory for new projects.
func DefaultProjectsBase() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(homeDir(), "Documents")
	case "windows":
		if profile := os.Getenv("USERPROFILE"); profile != "" {
			return filepath.Join(profile, "Documents")
		}
		return filepath.Join(homeDir(), "Documents")
	default:
		return filepath.Join(homeDir(), "Documents")
	}
}

// RegistryPath returns the path to the project registry file.
func RegistryPath() string {
	return filepath.Join(homeDir(), ".agentfab", "projects.json")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}
