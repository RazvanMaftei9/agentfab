package runtime

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type MaterializedTier interface {
	Path() string
	Refresh(context.Context) error
	Sync(context.Context) error
	Close() error
}

type StorageMaterializer interface {
	Materialize(ctx context.Context, tier StorageTier) (MaterializedTier, error)
}

type Workspace struct {
	Scratch MaterializedTier
	Agent   MaterializedTier
	Shared  MaterializedTier
}

func OpenWorkspace(ctx context.Context, storage Storage) (*Workspace, error) {
	scratch, err := MaterializeTier(ctx, storage, TierScratch)
	if err != nil {
		return nil, err
	}
	agent, err := MaterializeTier(ctx, storage, TierAgent)
	if err != nil {
		_ = scratch.Close()
		return nil, err
	}
	shared, err := MaterializeTier(ctx, storage, TierShared)
	if err != nil {
		_ = agent.Close()
		_ = scratch.Close()
		return nil, err
	}
	return &Workspace{
		Scratch: scratch,
		Agent:   agent,
		Shared:  shared,
	}, nil
}

func (w *Workspace) TierPaths() []string {
	if w == nil {
		return nil
	}
	return []string{
		w.Scratch.Path(),
		w.Agent.Path(),
		w.Shared.Path(),
	}
}

func (w *Workspace) Sync(ctx context.Context) error {
	if w == nil {
		return nil
	}
	for _, tier := range []MaterializedTier{w.Scratch, w.Agent, w.Shared} {
		if tier == nil {
			continue
		}
		if err := tier.Sync(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) Refresh(ctx context.Context) error {
	if w == nil {
		return nil
	}
	for _, tier := range []MaterializedTier{w.Shared, w.Agent, w.Scratch} {
		if tier == nil {
			continue
		}
		if err := tier.Refresh(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (w *Workspace) Close() error {
	if w == nil {
		return nil
	}
	var firstErr error
	for _, tier := range []MaterializedTier{w.Shared, w.Agent, w.Scratch} {
		if tier == nil {
			continue
		}
		if err := tier.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func MaterializeTier(ctx context.Context, storage Storage, tier StorageTier) (MaterializedTier, error) {
	if materializer, ok := storage.(StorageMaterializer); ok {
		return materializer.Materialize(ctx, tier)
	}
	return newStagedTier(ctx, storage, tier)
}

type stagedTier struct {
	storage Storage
	tier    StorageTier
	dir     string
	files   map[string]struct{}
}

func newStagedTier(ctx context.Context, storage Storage, tier StorageTier) (MaterializedTier, error) {
	dir, err := os.MkdirTemp("", "agentfab-tier-*")
	if err != nil {
		return nil, err
	}

	files, err := storage.ListAll(ctx, tier, "")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	stage := &stagedTier{
		storage: storage,
		tier:    tier,
		dir:     dir,
		files:   make(map[string]struct{}, len(files)),
	}
	if err := stage.refreshFromStorage(ctx, files); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	return stage, nil
}

func (s *stagedTier) Path() string {
	return s.dir
}

func (s *stagedTier) Refresh(ctx context.Context) error {
	files, err := s.storage.ListAll(ctx, s.tier, "")
	if err != nil {
		return err
	}
	return s.refreshFromStorage(ctx, files)
}

func (s *stagedTier) refreshFromStorage(ctx context.Context, files []string) error {
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}

	known := make(map[string]struct{}, len(files))
	for _, rel := range files {
		data, err := s.storage.Read(ctx, s.tier, rel)
		if err != nil {
			return err
		}
		dest := filepath.Join(s.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return err
		}
		known[rel] = struct{}{}
	}
	s.files = known
	return nil
}

func (s *stagedTier) Sync(ctx context.Context) error {
	current := make(map[string]struct{})
	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := s.storage.Write(ctx, s.tier, rel, data); err != nil {
			return err
		}
		current[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}

	for rel := range s.files {
		if _, ok := current[rel]; ok {
			continue
		}
		if err := s.storage.Delete(ctx, s.tier, rel); err != nil {
			return err
		}
	}

	s.files = current
	return nil
}

func (s *stagedTier) Close() error {
	return os.RemoveAll(s.dir)
}

func WalkMaterializedFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func HasPathPrefix(path, prefix string) bool {
	prefix = filepath.ToSlash(strings.Trim(prefix, "/"))
	path = filepath.ToSlash(strings.Trim(path, "/"))
	if prefix == "" {
		return true
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
