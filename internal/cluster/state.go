package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ClusterState holds cluster membership, persisted as JSON.
type ClusterState struct {
	Members []MemberInfo `json:"members"`
}

type MemberInfo struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"`         // "conductor" or "agent"
	Address     string    `json:"address"`
	PID         int       `json:"pid"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	StartedAt   time.Time `json:"started_at"`
}

// LoadClusterState reads cluster state from path (returns empty if absent).
func LoadClusterState(path string) (*ClusterState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ClusterState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cluster state: %w", err)
	}
	var state ClusterState
	if err := json.Unmarshal(data, &state); err != nil {
		// If the file is corrupted (e.g. from a prior race), log and
		// treat as empty so the next write can repair it.
		return &ClusterState{}, nil
	}
	return &state, nil
}

// SaveClusterState writes cluster state atomically via temp file + rename.
func SaveClusterState(path string, state *ClusterState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cluster state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cluster-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func lockClusterState(path string) (*os.File, error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	return f, nil
}

func unlockClusterState(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

func RegisterMember(path string, info MemberInfo) error {
	lock, err := lockClusterState(path)
	if err != nil {
		return err
	}
	defer unlockClusterState(lock)

	state, err := LoadClusterState(path)
	if err != nil {
		return err
	}
	found := false
	for i, m := range state.Members {
		if m.Name == info.Name {
			state.Members[i] = info
			found = true
			break
		}
	}
	if !found {
		state.Members = append(state.Members, info)
	}
	return SaveClusterState(path, state)
}

func DeregisterMember(path string, name string) error {
	lock, err := lockClusterState(path)
	if err != nil {
		return err
	}
	defer unlockClusterState(lock)

	state, err := LoadClusterState(path)
	if err != nil {
		return err
	}
	filtered := state.Members[:0]
	for _, m := range state.Members {
		if m.Name != name {
			filtered = append(filtered, m)
		}
	}
	state.Members = filtered
	return SaveClusterState(path, state)
}

func UpdateHeartbeat(path string, name string) error {
	lock, err := lockClusterState(path)
	if err != nil {
		return err
	}
	defer unlockClusterState(lock)

	state, err := LoadClusterState(path)
	if err != nil {
		return err
	}
	for i, m := range state.Members {
		if m.Name == name {
			state.Members[i].HeartbeatAt = time.Now()
			return SaveClusterState(path, state)
		}
	}
	return fmt.Errorf("member %q not found", name)
}

func DeadMembers(state *ClusterState, threshold time.Duration) []MemberInfo {
	now := time.Now()
	var dead []MemberInfo
	for _, m := range state.Members {
		if now.Sub(m.HeartbeatAt) > threshold {
			dead = append(dead, m)
		}
	}
	return dead
}

func AliveMembers(state *ClusterState, threshold time.Duration) []MemberInfo {
	now := time.Now()
	var alive []MemberInfo
	for _, m := range state.Members {
		if now.Sub(m.HeartbeatAt) <= threshold {
			alive = append(alive, m)
		}
	}
	return alive
}

// PurgeUnknownMembers removes members from the state file that are not in the
// known set. This cleans up stale entries from previous runs with different agents.
func PurgeUnknownMembers(path string, known map[string]bool) error {
	lock, err := lockClusterState(path)
	if err != nil {
		return err
	}
	defer unlockClusterState(lock)

	state, err := LoadClusterState(path)
	if err != nil {
		return err
	}
	filtered := state.Members[:0]
	for _, m := range state.Members {
		if known[m.Name] {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) == len(state.Members) {
		return nil // nothing to purge
	}
	state.Members = filtered
	return SaveClusterState(path, state)
}

func StatePath(dataDir string) string {
	return filepath.Join(dataDir, "shared", "cluster.json")
}
