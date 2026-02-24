package cluster

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMonitorRegistersAndDeregisters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")

	ctx, cancel := context.WithCancel(context.Background())

	m := &Monitor{
		Self: MemberInfo{
			Name:    "test-agent",
			Role:    "agent",
			Address: "localhost:50100",
			PID:     12345,
		},
		StatePath:     path,
		HeartbeatFreq: 50 * time.Millisecond,
		FailThreshold: 200 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for registration.
	time.Sleep(100 * time.Millisecond)

	state, err := LoadClusterState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if len(state.Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(state.Members))
	}
	if state.Members[0].Name != "test-agent" {
		t.Errorf("name: got %q", state.Members[0].Name)
	}

	// Cancel and wait for deregistration.
	cancel()
	<-done

	state, _ = LoadClusterState(path)
	if len(state.Members) != 0 {
		t.Errorf("expected 0 members after deregister, got %d", len(state.Members))
	}
}

func TestMonitorDetectsDeadMember(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.json")

	// Pre-register a "dead" member with an old heartbeat.
	dead := MemberInfo{
		Name:        "dead-agent",
		Role:        "agent",
		Address:     "localhost:50200",
		PID:         999,
		HeartbeatAt: time.Now().Add(-1 * time.Minute),
		StartedAt:   time.Now().Add(-2 * time.Minute),
	}
	if err := RegisterMember(path, dead); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var detectedDead []string

	ctx, cancel := context.WithCancel(context.Background())

	m := &Monitor{
		Self: MemberInfo{
			Name:    "monitor-agent",
			Role:    "agent",
			Address: "localhost:50100",
			PID:     1000,
		},
		StatePath:     path,
		HeartbeatFreq: 50 * time.Millisecond,
		FailThreshold: 200 * time.Millisecond,
		OnMemberDead: func(info MemberInfo) {
			mu.Lock()
			detectedDead = append(detectedDead, info.Name)
			mu.Unlock()
		},
	}

	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	// Wait for at least one tick.
	time.Sleep(150 * time.Millisecond)

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, name := range detectedDead {
		if name == "dead-agent" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected dead-agent to be detected, got", detectedDead)
	}
}
