package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestMemoryStoreLeaderLifecycle(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()

	lease, acquired, err := store.AcquireLeader(ctx, "conductor-a", "10.0.0.1:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader lease to be acquired")
	}
	if lease.Epoch != 1 {
		t.Fatalf("epoch = %d, want 1", lease.Epoch)
	}

	current, ok, err := store.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader returned error: %v", err)
	}
	if !ok {
		t.Fatal("expected a current leader")
	}
	if current.HolderID != "conductor-a" {
		t.Fatalf("leader holder = %q, want conductor-a", current.HolderID)
	}

	renewed, err := store.RenewLeader(ctx, lease, 5*time.Second)
	if err != nil {
		t.Fatalf("RenewLeader returned error: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatal("expected renew to extend lease")
	}

	if err := store.ReleaseLeader(ctx, renewed); err != nil {
		t.Fatalf("ReleaseLeader returned error: %v", err)
	}

	_, ok, err = store.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader after release returned error: %v", err)
	}
	if ok {
		t.Fatal("expected no current leader after release")
	}

	reacquired, acquired, err := store.AcquireLeader(ctx, "conductor-b", "10.0.0.2:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("reacquire leader returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader reacquisition to succeed")
	}
	if reacquired.Epoch != renewed.Epoch+1 {
		t.Fatalf("reacquired epoch = %d, want %d", reacquired.Epoch, renewed.Epoch+1)
	}
}

func TestMemoryStoreTaskLeaseOwnership(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()

	first, acquired, err := store.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-a",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireTaskLease returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected first task lease acquisition to succeed")
	}

	blocked, acquired, err := store.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-b",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("second AcquireTaskLease returned error: %v", err)
	}
	if acquired {
		t.Fatal("expected competing task lease acquisition to fail")
	}
	if blocked.OwnerID != "conductor-a" {
		t.Fatalf("blocking lease owner = %q, want conductor-a", blocked.OwnerID)
	}

	renewed, err := store.RenewTaskLease(ctx, first, 5*time.Second)
	if err != nil {
		t.Fatalf("RenewTaskLease returned error: %v", err)
	}
	if renewed.Epoch != first.Epoch {
		t.Fatalf("renewed epoch = %d, want %d", renewed.Epoch, first.Epoch)
	}

	if err := store.ReleaseTaskLease(ctx, renewed); err != nil {
		t.Fatalf("ReleaseTaskLease returned error: %v", err)
	}

	_, ok, err := store.GetTaskLease(ctx, "req-1", "task-1")
	if err != nil {
		t.Fatalf("GetTaskLease returned error: %v", err)
	}
	if ok {
		t.Fatal("expected lease to be released")
	}

	second, acquired, err := store.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-c",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("reacquire task lease returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected task lease reacquisition to succeed")
	}
	if second.Epoch != renewed.Epoch+1 {
		t.Fatalf("reacquired task lease epoch = %d, want %d", second.Epoch, renewed.Epoch+1)
	}
}

func TestMemoryStoreProfileLeaseOwnership(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()

	first, acquired, err := store.AcquireProfileLease(ctx, ProfileLease{
		Profile:          "developer",
		AssignedInstance: "node-a/developer/1",
		ExecutionNode:    "node-a",
		OwnerID:          "conductor-a",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireProfileLease returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected first profile lease acquisition to succeed")
	}

	blocked, acquired, err := store.AcquireProfileLease(ctx, ProfileLease{
		Profile:          "developer",
		AssignedInstance: "node-b/developer/1",
		ExecutionNode:    "node-b",
		OwnerID:          "conductor-b",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("competing AcquireProfileLease returned error: %v", err)
	}
	if acquired {
		t.Fatal("expected competing profile lease acquisition to fail")
	}
	if blocked.AssignedInstance != first.AssignedInstance {
		t.Fatalf("blocking lease instance = %q, want %q", blocked.AssignedInstance, first.AssignedInstance)
	}

	renewed, err := store.RenewProfileLease(ctx, first, 5*time.Second)
	if err != nil {
		t.Fatalf("RenewProfileLease returned error: %v", err)
	}
	if renewed.Epoch != first.Epoch {
		t.Fatalf("renewed epoch = %d, want %d", renewed.Epoch, first.Epoch)
	}

	if err := store.ReleaseProfileLease(ctx, renewed); err != nil {
		t.Fatalf("ReleaseProfileLease returned error: %v", err)
	}

	second, acquired, err := store.AcquireProfileLease(ctx, ProfileLease{
		Profile:          "developer",
		AssignedInstance: "node-b/developer/1",
		ExecutionNode:    "node-b",
		OwnerID:          "conductor-c",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("reacquire profile lease returned error: %v", err)
	}
	if !acquired {
		t.Fatal("expected profile lease reacquisition to succeed")
	}
	if second.Epoch != renewed.Epoch+1 {
		t.Fatalf("reacquired profile lease epoch = %d, want %d", second.Epoch, renewed.Epoch+1)
	}
}

func TestMemoryStoreExpiresAndRecoversMembership(t *testing.T) {
	store := NewMemoryStore("test-fabric")
	ctx := context.Background()
	staleAt := time.Now().UTC().Add(-NodeHeartbeatTTL - time.Second)

	if err := store.RegisterNode(ctx, Node{
		ID:              "node-a",
		State:           NodeStateReady,
		LastHeartbeatAt: staleAt,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	if err := store.RegisterInstance(ctx, AgentInstance{
		ID:              "node-a/developer/1",
		Profile:         "developer",
		NodeID:          "node-a",
		State:           InstanceStateReady,
		LastHeartbeatAt: staleAt,
	}); err != nil {
		t.Fatalf("RegisterInstance: %v", err)
	}

	node, ok, err := store.GetNode(ctx, "node-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected node to exist")
	}
	if node.State != NodeStateUnavailable {
		t.Fatalf("node state = %q, want %q", node.State, NodeStateUnavailable)
	}

	instance, ok, err := store.GetInstance(ctx, "node-a/developer/1")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if !ok {
		t.Fatal("expected instance to exist")
	}
	if instance.State != InstanceStateUnavailable {
		t.Fatalf("instance state = %q, want %q", instance.State, InstanceStateUnavailable)
	}

	now := time.Now().UTC()
	if err := store.HeartbeatNode(ctx, "node-a", now); err != nil {
		t.Fatalf("HeartbeatNode: %v", err)
	}
	if err := store.HeartbeatInstance(ctx, "node-a/developer/1", now); err != nil {
		t.Fatalf("HeartbeatInstance: %v", err)
	}

	node, ok, err = store.GetNode(ctx, "node-a")
	if err != nil {
		t.Fatalf("GetNode after heartbeat: %v", err)
	}
	if !ok || node.State != NodeStateReady {
		t.Fatalf("node after heartbeat = %+v, want ready", node)
	}

	instance, ok, err = store.GetInstance(ctx, "node-a/developer/1")
	if err != nil {
		t.Fatalf("GetInstance after heartbeat: %v", err)
	}
	if !ok || instance.State != InstanceStateReady {
		t.Fatalf("instance after heartbeat = %+v, want ready", instance)
	}
}
