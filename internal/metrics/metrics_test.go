package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFromDebugDir(t *testing.T) {
	dir := t.TempDir()

	// Create agent subdirectory with output.jsonl.
	agentDir := filepath.Join(dir, "developer")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}

	records := []struct {
		Timestamp    time.Time `json:"ts"`
		CallID       int64     `json:"call_id"`
		Model        string    `json:"model"`
		DurationMs   int64     `json:"duration_ms"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
		FinishReason string    `json:"finish_reason"`
	}{
		{time.Now(), 1, "anthropic/claude-sonnet-4-20250514", 2500, 1000, 500, "stop"},
		{time.Now(), 2, "anthropic/claude-sonnet-4-20250514", 3000, 2000, 800, "stop"},
	}

	f, err := os.Create(filepath.Join(agentDir, "output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, r := range records {
		enc.Encode(r)
	}
	f.Close()

	report, err := LoadFromDebugDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if report.TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2", report.TotalCalls)
	}
	if report.InputTokens != 3000 {
		t.Errorf("InputTokens = %d, want 3000", report.InputTokens)
	}
	if report.OutputTokens != 1300 {
		t.Errorf("OutputTokens = %d, want 1300", report.OutputTokens)
	}
	if report.TotalTokens != 4300 {
		t.Errorf("TotalTokens = %d, want 4300", report.TotalTokens)
	}
	if len(report.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(report.Agents))
	}
	if report.Agents[0].Agent != "developer" {
		t.Errorf("Agent = %q, want developer", report.Agents[0].Agent)
	}
	if report.Agents[0].AvgLatencyMs != 2750 {
		t.Errorf("AvgLatencyMs = %d, want 2750", report.Agents[0].AvgLatencyMs)
	}
	if len(report.Models) != 1 {
		t.Fatalf("len(Models) = %d, want 1", len(report.Models))
	}
	if report.EstCostUSD <= 0 {
		t.Error("EstCostUSD should be > 0")
	}
}

func TestLoadFromDebugDir_MultiAgent(t *testing.T) {
	dir := t.TempDir()

	for _, agent := range []string{"architect", "designer"} {
		agentDir := filepath.Join(dir, agent)
		os.MkdirAll(agentDir, 0755)

		f, _ := os.Create(filepath.Join(agentDir, "output.jsonl"))
		enc := json.NewEncoder(f)
		enc.Encode(map[string]any{
			"ts":            time.Now(),
			"call_id":       1,
			"model":         "anthropic/claude-opus-4",
			"duration_ms":   5000,
			"input_tokens":  10000,
			"output_tokens": 3000,
		})
		f.Close()
	}

	report, err := LoadFromDebugDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if report.TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2", report.TotalCalls)
	}
	if len(report.Agents) != 2 {
		t.Errorf("len(Agents) = %d, want 2", len(report.Agents))
	}
	if len(report.Models) != 1 {
		t.Errorf("len(Models) = %d, want 1 (both use same model)", len(report.Models))
	}
}

func TestLoadFromDebugDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	report, err := LoadFromDebugDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalCalls != 0 {
		t.Errorf("TotalCalls = %d, want 0", report.TotalCalls)
	}
}

func TestEstimateCost(t *testing.T) {
	tests := []struct {
		model  string
		input  int64
		output int64
		minUSD float64
		maxUSD float64
	}{
		{"anthropic/claude-sonnet-4-20250514", 1_000_000, 100_000, 4.0, 5.0},
		{"anthropic/claude-haiku-4-5-20250929", 1_000_000, 100_000, 0.3, 0.5},
		{"anthropic/claude-opus-4", 1_000_000, 100_000, 20.0, 25.0},
		{"openai/gpt-4o", 1_000_000, 100_000, 3.0, 4.0},
	}
	for _, tt := range tests {
		cost := estimateCost(tt.model, tt.input, tt.output, 0)
		if cost < tt.minUSD || cost > tt.maxUSD {
			t.Errorf("estimateCost(%q, %d, %d, 0) = %.4f, want [%.1f, %.1f]",
				tt.model, tt.input, tt.output, cost, tt.minUSD, tt.maxUSD)
		}
	}
}
