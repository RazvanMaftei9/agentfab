package controlplane

import (
	"fmt"
	"strings"
	"time"
)

type Backend string

const (
	BackendFile Backend = "file"
	BackendEtcd Backend = "etcd"
)

type BackendOptions struct {
	Backend Backend
	BaseDir string
	Fabric  string
	Etcd    EtcdOptions
}

type EtcdOptions struct {
	Endpoints   []string
	DialTimeout time.Duration
}

func NewStore(opts BackendOptions) (Store, error) {
	switch normalizeBackend(opts.Backend) {
	case "", BackendFile:
		return NewFileStore(opts.BaseDir, opts.Fabric)
	case BackendEtcd:
		return NewEtcdStore(opts.Fabric, opts.Etcd)
	default:
		return nil, fmt.Errorf("unsupported control-plane backend %q", opts.Backend)
	}
}

func normalizeBackend(backend Backend) Backend {
	return Backend(strings.ToLower(strings.TrimSpace(string(backend))))
}
