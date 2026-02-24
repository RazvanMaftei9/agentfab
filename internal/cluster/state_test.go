package cluster

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClusterStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared", "cluster.json")

	// Initially empty.
	state, err := LoadClusterState(path)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(state.Members) != 0 {
		t.Fatalf("expected 0 members, got %d", len(state.Members))
	}

	// Register two members.
	now := time.Now()
	info1 := MemberInfo{
		Name:        "conductor",
		Role:        "conductor",
		Address:     "localhost:50050",
		PID:         1000,
		HeartbeatAt: now,
		StartedAt:   now,
	}
	info2 := MemberInfo{
		Name:        "developer",
		Role:        "agent",
		Address:     "localhost:50100",
		PID:         1001,
		HeartbeatAt: now,
		StartedAt:   now,
	}

	if err := RegisterMember(path, info1); err != nil {
		t.Fatalf("register conductor: %v", err)
	}
	if err := RegisterMember(path, info2); err != nil {
		t.Fatalf("register developer: %v", err)
	}

	// Reload and verify.
	state, err = LoadClusterState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(state.Members))
	}

	// Update heartbeat.
	time.Sleep(10 * time.Millisecond)
	if err := UpdateHeartbeat(path, "conductor"); err != nil {
		t.Fatalf("update heartbeat: %v", err)
	}
	state, _ = LoadClusterState(path)
	for _, m := range state.Members {
		if m.Name == "conductor" && !m.HeartbeatAt.After(now) {
			t.Error("conductor heartbeat should be after original time")
		}
	}

	// Deregister.
	if err := DeregisterMember(path, "developer"); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	state, _ = LoadClusterState(path)
	if len(state.Members) != 1 {
		t.Fatalf("expected 1 member after deregister, got %d", len(state.Members))
	}
	if state.Members[0].Name != "conductor" {
		t.Errorf("remaining member should be conductor, got %q", state.Members[0].Name)
	}
}

func TestDeadMembers(t *testing.T) {
	now := time.Now()
	state := &ClusterState{
		Members: []MemberInfo{
			{Name: "alive", HeartbeatAt: now},
			{Name: "dead", HeartbeatAt: now.Add(-30 * time.Second)},
			{Name: "barely-alive", HeartbeatAt: now.Add(-14 * time.Second)},
		},
	}

	dead := DeadMembers(state, 15*time.Second)
	if len(dead) != 1 {
		t.Fatalf("expected 1 dead member, got %d", len(dead))
	}
	if dead[0].Name != "dead" {
		t.Errorf("expected dead member 'dead', got %q", dead[0].Name)
	}

	alive := AliveMembers(state, 15*time.Second)
	if len(alive) != 2 {
		t.Fatalf("expected 2 alive members, got %d", len(alive))
	}
}

func TestStatePath(t *testing.T) {
	got := StatePath("/data")
	want := filepath.Join("/data", "shared", "cluster.json")
	if got != want {
		t.Errorf("StatePath: got %q, want %q", got, want)
	}
}

func TestRegisterMemberUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")

	now := time.Now()
	info := MemberInfo{Name: "agent-a", Role: "agent", PID: 100, HeartbeatAt: now}
	if err := RegisterMember(path, info); err != nil {
		t.Fatal(err)
	}

	// Update same member.
	info.PID = 200
	if err := RegisterMember(path, info); err != nil {
		t.Fatal(err)
	}

	state, _ := LoadClusterState(path)
	if len(state.Members) != 1 {
		t.Fatalf("expected 1 member (update, not duplicate), got %d", len(state.Members))
	}
	if state.Members[0].PID != 200 {
		t.Errorf("PID should be updated to 200, got %d", state.Members[0].PID)
	}
}

func TestUpdateHeartbeatNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")

	// Create empty state file.
	if err := SaveClusterState(path, &ClusterState{}); err != nil {
		t.Fatal(err)
	}

	err := UpdateHeartbeat(path, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent member")
	}
}

func TestLoadClusterStateInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")
	os.WriteFile(path, []byte("not json"), 0644)

	// Corrupted JSON is treated as empty state (self-healing) rather than an error,
	// so the next write can repair the file.
	state, err := LoadClusterState(path)
	if err != nil {
		t.Errorf("expected no error for corrupted JSON, got: %v", err)
	}
	if len(state.Members) != 0 {
		t.Errorf("expected empty members, got %d", len(state.Members))
	}
}
