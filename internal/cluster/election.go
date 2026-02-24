package cluster

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync/atomic"
	"time"
)

// ElectAndRespawn elects a new conductor (alphabetically first alive agent wins).
func ElectAndRespawn(statePath string, self MemberInfo, binary string, conductorArgs []string, timeout time.Duration) error {
	state, err := LoadClusterState(statePath)
	if err != nil {
		return fmt.Errorf("election: load state: %w", err)
	}

	alive := AliveMembers(state, 15*time.Second)
	var agents []MemberInfo
	for _, m := range alive {
		if m.Role == "agent" {
			agents = append(agents, m)
		}
	}

	if len(agents) == 0 {
		return fmt.Errorf("election: no alive agents")
	}

	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Name < agents[j].Name
	})
	elector := agents[0]

	slog.Info("conductor election", "elector", elector.Name, "self", self.Name)

	if elector.Name == self.Name {
		return spawnConductor(binary, conductorArgs)
	}

	return waitForConductor(statePath, timeout)
}

func spawnConductor(binary string, args []string) error {
	cmd := exec.Command(binary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn conductor: %w", err)
	}

	slog.Info("conductor respawned", "pid", cmd.Process.Pid)
	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

func waitForConductor(statePath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := LoadClusterState(statePath)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		for _, m := range state.Members {
			if m.Role == "conductor" && time.Since(m.HeartbeatAt) < 15*time.Second {
				slog.Info("conductor recovered", "name", m.Name, "address", m.Address)
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timeout waiting for conductor to respawn")
}

// ConductorDeadCallback triggers conductor election when the conductor dies.
func ConductorDeadCallback(statePath string, self MemberInfo, binary string, conductorArgs []string) func(MemberInfo) {
	var electionInProgress atomic.Bool
	return func(dead MemberInfo) {
		if dead.Role != "conductor" {
			return
		}
		if !electionInProgress.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer electionInProgress.Store(false)
			if err := ElectAndRespawn(statePath, self, binary, conductorArgs, 60*time.Second); err != nil {
				slog.Error("conductor election failed", "error", err)
			}
		}()
	}
}
