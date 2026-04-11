package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

var _ runtime.Storage = (*Storage)(nil)
var _ runtime.StorageMaterializer = (*Storage)(nil)

type Storage struct {
	agentName string
	layout    runtime.StorageLayout
}

// NewStorage creates a local filesystem storage rooted at baseDir.
// agentName scopes shared-volume writes to artifacts/{agentName}/.
func NewStorage(baseDir, agentName string) *Storage {
	return NewStorageWithLayout(runtime.StorageLayout{
		SharedRoot:  filepath.Join(baseDir, "shared"),
		AgentRoot:   filepath.Join(baseDir, "agents"),
		ScratchRoot: os.TempDir(),
	}, agentName)
}

func NewStorageWithLayout(layout runtime.StorageLayout, agentName string) *Storage {
	return &Storage{
		agentName: agentName,
		layout:    layout,
	}
}

func (s *Storage) tierPath(tier runtime.StorageTier) string {
	switch tier {
	case runtime.TierShared:
		return s.layout.SharedRoot
	case runtime.TierAgent:
		return filepath.Join(s.layout.AgentRoot, s.agentName)
	case runtime.TierScratch:
		return filepath.Join(s.layout.ScratchRoot, "agentfab-"+s.agentName)
	default:
		return s.layout.SharedRoot
	}
}

func (s *Storage) fullPath(tier runtime.StorageTier, path string) (string, error) {
	base := s.tierPath(tier)
	full := filepath.Join(base, path)

	rel, err := filepath.Rel(base, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q escapes storage tier", path)
	}

	return full, nil
}

func (s *Storage) checkWriteScope(tier runtime.StorageTier, path string) error {
	if tier != runtime.TierShared {
		return nil
	}

	clean := filepath.Clean(path)
	allowed := []string{
		"artifacts/" + s.agentName + "/",
		"logs/",
		"agents.yaml",
		"knowledge/",
	}

	for _, prefix := range allowed {
		if strings.HasPrefix(clean, prefix) || clean == strings.TrimSuffix(prefix, "/") {
			return nil
		}
	}

	return fmt.Errorf("agent %q cannot write to shared path %q", s.agentName, path)
}

func (s *Storage) Read(_ context.Context, tier runtime.StorageTier, path string) ([]byte, error) {
	full, err := s.fullPath(tier, path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (s *Storage) Write(_ context.Context, tier runtime.StorageTier, path string, data []byte) error {
	if err := s.checkWriteScope(tier, path); err != nil {
		return err
	}
	full, err := s.fullPath(tier, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	return os.WriteFile(full, data, 0644)
}

func (s *Storage) Append(_ context.Context, tier runtime.StorageTier, path string, data []byte) error {
	if err := s.checkWriteScope(tier, path); err != nil {
		return err
	}
	full, err := s.fullPath(tier, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.OpenFile(full, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (s *Storage) List(_ context.Context, tier runtime.StorageTier, pattern string) ([]string, error) {
	full, err := s.fullPath(tier, pattern)
	if err != nil {
		return nil, err
	}
	base := s.tierPath(tier)
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, err
	}

	result := make([]string, 0, len(matches))
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		rel, err := filepath.Rel(base, m)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		result = append(result, rel)
	}
	return result, nil
}

func (s *Storage) ListAll(_ context.Context, tier runtime.StorageTier, prefix string) ([]string, error) {
	base := s.tierPath(tier)
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, err
	}

	prefix = filepath.ToSlash(strings.Trim(filepath.Clean(prefix), "/"))
	if prefix == "." {
		prefix = ""
	}

	var result []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if prefix != "" && rel != prefix && !strings.HasPrefix(rel, prefix+"/") {
			return nil
		}
		result = append(result, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Storage) SharedDir() string {
	return s.tierPath(runtime.TierShared)
}

func (s *Storage) TierDir(tier runtime.StorageTier) string {
	return s.tierPath(tier)
}

func (s *Storage) Materialize(_ context.Context, tier runtime.StorageTier) (runtime.MaterializedTier, error) {
	root := s.tierPath(tier)
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	return directTier{path: root}, nil
}

func (s *Storage) checkDeleteScope(tier runtime.StorageTier) error {
	switch tier {
	case runtime.TierScratch:
		return nil
	case runtime.TierAgent:
		return fmt.Errorf("delete blocked on agent tier: operation requires user consent")
	case runtime.TierShared:
		return fmt.Errorf("delete blocked on shared tier: operation requires user consent")
	default:
		return fmt.Errorf("delete blocked on unknown tier")
	}
}

func (s *Storage) Delete(_ context.Context, tier runtime.StorageTier, path string) error {
	if err := s.checkDeleteScope(tier); err != nil {
		return err
	}
	if err := s.checkWriteScope(tier, path); err != nil {
		return err
	}
	full, err := s.fullPath(tier, path)
	if err != nil {
		return err
	}
	return os.RemoveAll(full)
}

func (s *Storage) Exists(_ context.Context, tier runtime.StorageTier, path string) (bool, error) {
	full, err := s.fullPath(tier, path)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(full)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

type directTier struct {
	path string
}

func (d directTier) Path() string {
	return d.path
}

func (d directTier) Refresh(context.Context) error {
	return nil
}

func (d directTier) Sync(context.Context) error {
	return nil
}

func (d directTier) Close() error {
	return nil
}
