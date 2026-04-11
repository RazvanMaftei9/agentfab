package controlplane

import (
	"context"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/etcd/server/v3/embed"
)

func TestEtcdStoreLeaderAndTaskLeaseLifecycle(t *testing.T) {
	store, cleanup := newTestEtcdStore(t)
	defer cleanup()

	ctx := context.Background()

	lease, acquired, err := store.AcquireLeader(ctx, "conductor-a", "127.0.0.1:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader acquisition")
	}

	renewed, err := store.RenewLeader(ctx, lease, 5*time.Second)
	if err != nil {
		t.Fatalf("RenewLeader: %v", err)
	}
	if renewed.Epoch != lease.Epoch {
		t.Fatalf("renewed epoch = %d, want %d", renewed.Epoch, lease.Epoch)
	}

	taskLease, acquired, err := store.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-a",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireTaskLease: %v", err)
	}
	if !acquired {
		t.Fatal("expected task lease acquisition")
	}

	if err := store.ReleaseTaskLease(ctx, taskLease); err != nil {
		t.Fatalf("ReleaseTaskLease: %v", err)
	}
	if err := store.ReleaseLeader(ctx, renewed); err != nil {
		t.Fatalf("ReleaseLeader: %v", err)
	}
}

func TestEtcdStorePersistsRequestsTasksAndMembership(t *testing.T) {
	store, cleanup := newTestEtcdStore(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.RegisterNode(ctx, Node{
		ID:              "node-a",
		State:           NodeStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if err := store.RegisterInstance(ctx, AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		State:           InstanceStateReady,
		LastHeartbeatAt: now,
	}); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}
	if err := store.UpsertRequest(ctx, RequestRecord{
		ID:          "req-1",
		State:       RequestStateRunning,
		UserRequest: "Build something",
		LeaderID:    "conductor-a",
	}); err != nil {
		t.Fatalf("UpsertRequest: %v", err)
	}
	if err := store.UpsertTask(ctx, TaskRecord{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		Status:    "running",
	}); err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}

	node, ok, err := store.GetNode(ctx, "node-a")
	if err != nil || !ok {
		t.Fatalf("GetNode: ok=%v err=%v", ok, err)
	}
	if node.State != NodeStateReady {
		t.Fatalf("node state = %q, want ready", node.State)
	}

	instance, ok, err := store.GetInstance(ctx, "node-a/developer/1")
	if err != nil || !ok {
		t.Fatalf("GetInstance: ok=%v err=%v", ok, err)
	}
	if instance.Profile != "developer" {
		t.Fatalf("instance profile = %q, want developer", instance.Profile)
	}

	request, ok, err := store.GetRequest(ctx, "req-1")
	if err != nil || !ok {
		t.Fatalf("GetRequest: ok=%v err=%v", ok, err)
	}
	if request.State != RequestStateRunning {
		t.Fatalf("request state = %q, want running", request.State)
	}

	tasks, err := store.ListTasks(ctx, "req-1")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].TaskID != "task-1" {
		t.Fatalf("tasks = %+v, want task-1", tasks)
	}
}

func newTestEtcdStore(t *testing.T) (*EtcdStore, func()) {
	t.Helper()

	clientURL := mustURL(t, "http://"+freeAddress(t))
	peerURL := mustURL(t, "http://"+freeAddress(t))

	cfg := embed.NewConfig()
	cfg.Dir = filepath.Join(t.TempDir(), "etcd")
	cfg.Logger = "zap"
	cfg.LogOutputs = []string{"/dev/null"}
	cfg.ListenClientUrls = []url.URL{*clientURL}
	cfg.AdvertiseClientUrls = []url.URL{*clientURL}
	cfg.ListenPeerUrls = []url.URL{*peerURL}
	cfg.AdvertisePeerUrls = []url.URL{*peerURL}
	cfg.InitialCluster = cfg.InitialClusterFromName(cfg.Name)

	server, err := embed.StartEtcd(cfg)
	if err != nil {
		t.Fatalf("StartEtcd: %v", err)
	}
	select {
	case <-server.Server.ReadyNotify():
	case <-time.After(10 * time.Second):
		server.Server.Stop()
		t.Fatal("timed out waiting for embedded etcd")
	}

	store, err := NewEtcdStore("test-fabric", EtcdOptions{
		Endpoints: []string{clientURL.String()},
	})
	if err != nil {
		server.Close()
		t.Fatalf("NewEtcdStore: %v", err)
	}

	cleanup := func() {
		_ = store.Close()
		server.Close()
	}
	return store, cleanup
}

func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", raw, err)
	}
	return parsed
}
