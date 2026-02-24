package cluster

import (
	"context"
	"log/slog"
	"time"
)

// Monitor periodically updates its own heartbeat and checks for dead members.
type Monitor struct {
	Self          MemberInfo
	StatePath     string
	HeartbeatFreq time.Duration   // How often to update own heartbeat (default 5s).
	FailThreshold time.Duration   // How long before a member is considered dead (default 15s).
	OnMemberDead  func(MemberInfo) // Callback when a dead member is detected.
	KnownMembers  map[string]bool  // If non-nil, only report dead members whose name is in this set.
}

// Run registers self, heartbeats periodically, and deregisters on ctx cancellation.
func (m *Monitor) Run(ctx context.Context) {
	if m.HeartbeatFreq == 0 {
		m.HeartbeatFreq = 5 * time.Second
	}
	if m.FailThreshold == 0 {
		m.FailThreshold = 15 * time.Second
	}

	// Purge stale members from previous runs before registering.
	if m.KnownMembers != nil {
		if err := PurgeUnknownMembers(m.StatePath, m.KnownMembers); err != nil {
			slog.Warn("cluster monitor: purge stale members failed", "error", err)
		}
	}

	m.Self.HeartbeatAt = time.Now()
	m.Self.StartedAt = time.Now()
	if err := RegisterMember(m.StatePath, m.Self); err != nil {
		slog.Error("cluster monitor: failed to register", "name", m.Self.Name, "error", err)
	}

	ticker := time.NewTicker(m.HeartbeatFreq)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if err := DeregisterMember(m.StatePath, m.Self.Name); err != nil {
				slog.Warn("cluster monitor: deregister failed", "name", m.Self.Name, "error", err)
			}
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

func (m *Monitor) tick() {
	if err := UpdateHeartbeat(m.StatePath, m.Self.Name); err != nil {
		slog.Warn("cluster monitor: heartbeat update failed", "name", m.Self.Name, "error", err)
	}

	if m.OnMemberDead == nil {
		return
	}
	state, err := LoadClusterState(m.StatePath)
	if err != nil {
		slog.Warn("cluster monitor: load state failed", "error", err)
		return
	}
	dead := DeadMembers(state, m.FailThreshold)
	for _, d := range dead {
		if d.Name == m.Self.Name {
			continue // Don't report ourselves as dead.
		}
		if m.KnownMembers != nil && !m.KnownMembers[d.Name] {
			continue // Stale member from a previous run; not in current fabric.
		}
		slog.Warn("cluster monitor: dead member detected", "name", d.Name, "role", d.Role,
			"last_heartbeat", d.HeartbeatAt)
		m.OnMemberDead(d)
	}
}
