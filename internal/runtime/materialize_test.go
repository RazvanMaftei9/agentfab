package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeStorage struct {
	files map[StorageTier]map[string][]byte
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		files: map[StorageTier]map[string][]byte{
			TierShared:  {},
			TierAgent:   {},
			TierScratch: {},
		},
	}
}

func (s *fakeStorage) Read(_ context.Context, tier StorageTier, path string) ([]byte, error) {
	data, ok := s.files[tier][path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStorage) Write(_ context.Context, tier StorageTier, path string, data []byte) error {
	s.files[tier][path] = append([]byte(nil), data...)
	return nil
}

func (s *fakeStorage) Append(_ context.Context, tier StorageTier, path string, data []byte) error {
	s.files[tier][path] = append(s.files[tier][path], data...)
	return nil
}

func (s *fakeStorage) List(_ context.Context, tier StorageTier, pattern string) ([]string, error) {
	return s.ListAll(context.Background(), tier, pattern)
}

func (s *fakeStorage) ListAll(_ context.Context, tier StorageTier, prefix string) ([]string, error) {
	var paths []string
	for path := range s.files[tier] {
		if HasPathPrefix(path, prefix) {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func (s *fakeStorage) Exists(_ context.Context, tier StorageTier, path string) (bool, error) {
	_, ok := s.files[tier][path]
	return ok, nil
}

func (s *fakeStorage) Delete(_ context.Context, tier StorageTier, path string) error {
	delete(s.files[tier], path)
	return nil
}

func (s *fakeStorage) TierDir(StorageTier) string {
	return ""
}

func (s *fakeStorage) SharedDir() string {
	return ""
}

func TestMaterializeTierStagesAndSyncsBack(t *testing.T) {
	ctx := context.Background()
	storage := newFakeStorage()
	if err := storage.Write(ctx, TierAgent, "docs/one.txt", []byte("one")); err != nil {
		t.Fatal(err)
	}

	tier, err := MaterializeTier(ctx, storage, TierAgent)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer tier.Close()

	stagedPath := filepath.Join(tier.Path(), "docs", "one.txt")
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatalf("read staged file: %v", err)
	}
	if string(data) != "one" {
		t.Fatalf("staged content = %q, want %q", data, "one")
	}

	if err := os.WriteFile(stagedPath, []byte("updated"), 0644); err != nil {
		t.Fatalf("update staged file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tier.Path(), "docs", "two.txt"), []byte("two"), 0644); err != nil {
		t.Fatalf("add staged file: %v", err)
	}

	if err := tier.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}

	updated, _ := storage.Read(ctx, TierAgent, "docs/one.txt")
	if string(updated) != "updated" {
		t.Fatalf("synced content = %q, want %q", updated, "updated")
	}
	second, _ := storage.Read(ctx, TierAgent, "docs/two.txt")
	if string(second) != "two" {
		t.Fatalf("second file = %q, want %q", second, "two")
	}
}

func TestMaterializeTierRefreshesFromStorage(t *testing.T) {
	ctx := context.Background()
	storage := newFakeStorage()
	if err := storage.Write(ctx, TierShared, "artifacts/demo.txt", []byte("v1")); err != nil {
		t.Fatal(err)
	}

	tier, err := MaterializeTier(ctx, storage, TierShared)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer tier.Close()

	if err := storage.Write(ctx, TierShared, "artifacts/demo.txt", []byte("v2")); err != nil {
		t.Fatal(err)
	}

	if err := tier.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tier.Path(), "artifacts", "demo.txt"))
	if err != nil {
		t.Fatalf("read refreshed file: %v", err)
	}
	if string(data) != "v2" {
		t.Fatalf("refreshed content = %q, want %q", data, "v2")
	}
}
