package controlplane

import (
	"fmt"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/config"
)

func BackendOptionsFromFabric(td *config.FabricDef, baseDir string) (BackendOptions, error) {
	opts := BackendOptions{
		Backend: BackendFile,
		BaseDir: baseDir,
	}
	if td == nil {
		return opts, fmt.Errorf("fabric definition is required")
	}
	opts.Fabric = td.Fabric.Name

	if backend := strings.TrimSpace(td.ControlPlane.Backend); backend != "" {
		opts.Backend = Backend(backend)
	}

	if len(td.ControlPlane.Etcd.Endpoints) > 0 {
		opts.Etcd.Endpoints = append([]string{}, td.ControlPlane.Etcd.Endpoints...)
	}
	if strings.TrimSpace(td.ControlPlane.Etcd.DialTimeout) != "" {
		dialTimeout, err := time.ParseDuration(td.ControlPlane.Etcd.DialTimeout)
		if err != nil {
			return BackendOptions{}, fmt.Errorf("parse control-plane etcd dial timeout: %w", err)
		}
		opts.Etcd.DialTimeout = dialTimeout
	}

	return opts, nil
}
