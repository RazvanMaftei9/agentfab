package local

import (
	"context"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// SharedAppender writes to the shared storage volume.
type SharedAppender struct {
	storage runtime.Storage
}

func NewSharedAppender(storage runtime.Storage) *SharedAppender {
	return &SharedAppender{storage: storage}
}

func (a *SharedAppender) Append(ctx context.Context, path string, data []byte) error {
	return a.storage.Append(ctx, runtime.TierShared, path, data)
}
