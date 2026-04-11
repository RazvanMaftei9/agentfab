package controlplane

import (
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/config"
)

func TestBackendOptionsFromFabricEtcd(t *testing.T) {
	td := config.DefaultFabricDef("test-fabric")
	td.ControlPlane = config.FabricControlPlane{
		Backend: "etcd",
		Etcd: config.FabricControlPlaneEtcd{
			Endpoints:   []string{"http://127.0.0.1:2379"},
			DialTimeout: "7s",
		},
	}

	opts, err := BackendOptionsFromFabric(td, "/tmp/agentfab")
	if err != nil {
		t.Fatalf("BackendOptionsFromFabric: %v", err)
	}

	if opts.Backend != BackendEtcd {
		t.Fatalf("backend = %q, want %q", opts.Backend, BackendEtcd)
	}
	if len(opts.Etcd.Endpoints) != 1 || opts.Etcd.Endpoints[0] != "http://127.0.0.1:2379" {
		t.Fatalf("endpoints = %v, want localhost etcd endpoint", opts.Etcd.Endpoints)
	}
	if opts.Etcd.DialTimeout != 7*time.Second {
		t.Fatalf("dial timeout = %s, want 7s", opts.Etcd.DialTimeout)
	}
}
