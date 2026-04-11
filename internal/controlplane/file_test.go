package controlplane

import (
	"context"
	"testing"
	"time"
)

func TestFileStorePersistsStateAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if err := store.RegisterNode(ctx, Node{
		ID:      "conductor-a",
		Address: "10.0.0.1:9443",
		State:   NodeStateReady,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	leader, acquired, err := store.AcquireLeader(ctx, "conductor-a", "10.0.0.1:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader lease acquisition to succeed")
	}

	if err := store.UpsertRequest(ctx, RequestRecord{
		ID:           "req-1",
		State:        RequestStateRunning,
		UserRequest:  "Build a service",
		GraphVersion: 1,
		LeaderID:     "conductor-a",
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
		t.Fatal("expected task lease acquisition to succeed")
	}

	restarted, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore after restart: %v", err)
	}

	node, ok, err := restarted.GetNode(ctx, "conductor-a")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if !ok {
		t.Fatal("expected node to persist")
	}
	if node.Address != "10.0.0.1:9443" {
		t.Fatalf("node address = %q, want 10.0.0.1:9443", node.Address)
	}

	currentLeader, ok, err := restarted.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader: %v", err)
	}
	if !ok {
		t.Fatal("expected leader to persist")
	}
	if currentLeader.HolderID != leader.HolderID {
		t.Fatalf("leader holder = %q, want %q", currentLeader.HolderID, leader.HolderID)
	}

	request, ok, err := restarted.GetRequest(ctx, "req-1")
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if !ok {
		t.Fatal("expected request to persist")
	}
	if request.State != RequestStateRunning {
		t.Fatalf("request state = %q, want %q", request.State, RequestStateRunning)
	}

	tasks, err := restarted.ListTasks(ctx, "req-1")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("task count = %d, want 1", len(tasks))
	}
	if tasks[0].TaskID != "task-1" {
		t.Fatalf("task ID = %q, want task-1", tasks[0].TaskID)
	}

	currentTaskLease, ok, err := restarted.GetTaskLease(ctx, "req-1", "task-1")
	if err != nil {
		t.Fatalf("GetTaskLease: %v", err)
	}
	if !ok {
		t.Fatal("expected task lease to persist")
	}
	if currentTaskLease.Epoch != taskLease.Epoch {
		t.Fatalf("task lease epoch = %d, want %d", currentTaskLease.Epoch, taskLease.Epoch)
	}
}

func TestFileStoreLeaseEpochsRemainMonotonic(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	firstLeader, acquired, err := store.AcquireLeader(ctx, "conductor-a", "10.0.0.1:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader acquisition to succeed")
	}
	if err := store.ReleaseLeader(ctx, firstLeader); err != nil {
		t.Fatalf("ReleaseLeader: %v", err)
	}

	restarted, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore after restart: %v", err)
	}

	secondLeader, acquired, err := restarted.AcquireLeader(ctx, "conductor-b", "10.0.0.2:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader after restart: %v", err)
	}
	if !acquired {
		t.Fatal("expected leader reacquisition to succeed")
	}
	if secondLeader.Epoch != firstLeader.Epoch+1 {
		t.Fatalf("leader epoch = %d, want %d", secondLeader.Epoch, firstLeader.Epoch+1)
	}

	firstTaskLease, acquired, err := restarted.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-b",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireTaskLease: %v", err)
	}
	if !acquired {
		t.Fatal("expected task lease acquisition to succeed")
	}
	if err := restarted.ReleaseTaskLease(ctx, firstTaskLease); err != nil {
		t.Fatalf("ReleaseTaskLease: %v", err)
	}

	again, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore second restart: %v", err)
	}

	secondTaskLease, acquired, err := again.AcquireTaskLease(ctx, TaskLease{
		RequestID: "req-1",
		TaskID:    "task-1",
		Profile:   "developer",
		OwnerID:   "conductor-c",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireTaskLease after restart: %v", err)
	}
	if !acquired {
		t.Fatal("expected task lease reacquisition to succeed")
	}
	if secondTaskLease.Epoch != firstTaskLease.Epoch+1 {
		t.Fatalf("task lease epoch = %d, want %d", secondTaskLease.Epoch, firstTaskLease.Epoch+1)
	}

	firstProfileLease, acquired, err := again.AcquireProfileLease(ctx, ProfileLease{
		Profile:          "developer",
		AssignedInstance: "node-a/developer/1",
		ExecutionNode:    "node-a",
		OwnerID:          "conductor-c",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireProfileLease: %v", err)
	}
	if !acquired {
		t.Fatal("expected profile lease acquisition to succeed")
	}
	if err := again.ReleaseProfileLease(ctx, firstProfileLease); err != nil {
		t.Fatalf("ReleaseProfileLease: %v", err)
	}

	finalStore, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore third restart: %v", err)
	}
	secondProfileLease, acquired, err := finalStore.AcquireProfileLease(ctx, ProfileLease{
		Profile:          "developer",
		AssignedInstance: "node-b/developer/1",
		ExecutionNode:    "node-b",
		OwnerID:          "conductor-d",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireProfileLease after restart: %v", err)
	}
	if !acquired {
		t.Fatal("expected profile lease reacquisition to succeed")
	}
	if secondProfileLease.Epoch != firstProfileLease.Epoch+1 {
		t.Fatalf("profile lease epoch = %d, want %d", secondProfileLease.Epoch, firstProfileLease.Epoch+1)
	}
}

func TestFileStoreSynchronizesAcrossStoreInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	first, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore first: %v", err)
	}
	second, err := NewFileStore(dir, "test-fabric")
	if err != nil {
		t.Fatalf("NewFileStore second: %v", err)
	}

	leader, acquired, err := first.AcquireLeader(ctx, "conductor-a", "10.0.0.1:9443", 5*time.Second)
	if err != nil {
		t.Fatalf("AcquireLeader: %v", err)
	}
	if !acquired {
		t.Fatal("expected first leader acquisition to succeed")
	}

	currentLeader, ok, err := second.GetLeader(ctx)
	if err != nil {
		t.Fatalf("GetLeader from second store: %v", err)
	}
	if !ok {
		t.Fatal("expected second store to observe current leader")
	}
	if currentLeader.HolderID != leader.HolderID {
		t.Fatalf("leader holder = %q, want %q", currentLeader.HolderID, leader.HolderID)
	}

	if err := second.UpsertRequest(ctx, RequestRecord{
		ID:           "req-shared",
		State:        RequestStateRunning,
		UserRequest:  "Build a service",
		GraphVersion: 1,
		LeaderID:     leader.HolderID,
	}); err != nil {
		t.Fatalf("UpsertRequest from second store: %v", err)
	}

	request, ok, err := first.GetRequest(ctx, "req-shared")
	if err != nil {
		t.Fatalf("GetRequest from first store: %v", err)
	}
	if !ok {
		t.Fatal("expected first store to observe shared request")
	}
	if request.State != RequestStateRunning {
		t.Fatalf("request state = %q, want %q", request.State, RequestStateRunning)
	}
}
