package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/razvanmaftei/agentfab/internal/loop"
)

type Checkpoint struct {
	AgentName   string            `json:"agent_name"`
	CurrentTask string            `json:"current_task,omitempty"`
	LoopStates  []loop.LoopState  `json:"loop_states,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	SleepState  *SleepState       `json:"sleep_state,omitempty"`
}

type SleepState struct {
	EnteredAt   time.Time `json:"entered_at"`
	CurationRun bool      `json:"curation_run"` // true if curation completed in this sleep cycle
}

func SaveCheckpoint(dir string, cp *Checkpoint) error {
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	path := filepath.Join(dir, "checkpoint.json")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// LoadCheckpoint reads a checkpoint, returning nil if it doesn't exist.
func LoadCheckpoint(dir string) (*Checkpoint, error) {
	path := filepath.Join(dir, "checkpoint.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}

	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	return &cp, nil
}
