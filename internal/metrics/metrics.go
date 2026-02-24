package metrics

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CallRecord is a single LLM call from a debug output.jsonl file.
type CallRecord struct {
	Agent           string    `json:"agent"`
	Model           string    `json:"model"`
	CallID          int64     `json:"call_id"`
	Timestamp       time.Time `json:"ts"`
	DurationMs      int64     `json:"duration_ms"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	CacheReadTokens int64     `json:"cache_read_tokens,omitempty"`
	FinishReason    string    `json:"finish_reason,omitempty"`
}

// AgentSummary aggregates usage for a single agent.
type AgentSummary struct {
	Agent           string  `json:"agent"`
	Model           string  `json:"model"`
	TotalCalls      int64   `json:"total_calls"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens,omitempty"`
	TotalTokens     int64   `json:"total_tokens"`
	TotalTimeMs     int64   `json:"total_time_ms"`
	AvgLatencyMs    int64   `json:"avg_latency_ms"`
	EstCostUSD      float64 `json:"est_cost_usd"`
}

// Report is the full metrics report.
type Report struct {
	GeneratedAt     time.Time      `json:"generated_at"`
	Agents          []AgentSummary `json:"agents"`
	Models          []ModelSummary `json:"models"`
	TotalCalls      int64          `json:"total_calls"`
	InputTokens     int64          `json:"input_tokens"`
	OutputTokens    int64          `json:"output_tokens"`
	CacheReadTokens int64          `json:"cache_read_tokens,omitempty"`
	TotalTokens     int64          `json:"total_tokens"`
	TotalTimeMs     int64          `json:"total_time_ms"`
	EstCostUSD      float64        `json:"est_cost_usd"`
}

// ModelSummary aggregates usage by model.
type ModelSummary struct {
	Model           string  `json:"model"`
	TotalCalls      int64   `json:"total_calls"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens,omitempty"`
	TotalTokens     int64   `json:"total_tokens"`
	EstCostUSD      float64 `json:"est_cost_usd"`
}

// LoadFromDebugDir reads all agent output.jsonl files from a debug directory
// and produces a usage report.
func LoadFromDebugDir(debugDir string) (*Report, error) {
	entries, err := os.ReadDir(debugDir)
	if err != nil {
		return nil, err
	}

	var records []CallRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		outputPath := filepath.Join(debugDir, agentName, "output.jsonl")
		agentRecords, err := readOutputJSONL(outputPath, agentName)
		if err != nil {
			continue // agent dir without output.jsonl is fine
		}
		records = append(records, agentRecords...)
	}

	return buildReport(records), nil
}

func readOutputJSONL(path, agent string) ([]CallRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var records []CallRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB line buffer
	for scanner.Scan() {
		var raw struct {
			Timestamp       time.Time `json:"ts"`
			CallID          int64     `json:"call_id"`
			Model           string    `json:"model"`
			DurationMs      int64     `json:"duration_ms"`
			InputTokens     int64     `json:"input_tokens"`
			OutputTokens    int64     `json:"output_tokens"`
			CacheReadTokens int64     `json:"cache_read_tokens"`
			FinishReason    string    `json:"finish_reason"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		records = append(records, CallRecord{
			Agent:           agent,
			Model:           raw.Model,
			CallID:          raw.CallID,
			Timestamp:       raw.Timestamp,
			DurationMs:      raw.DurationMs,
			InputTokens:     raw.InputTokens,
			OutputTokens:    raw.OutputTokens,
			CacheReadTokens: raw.CacheReadTokens,
			FinishReason:    raw.FinishReason,
		})
	}
	return records, nil
}

func buildReport(records []CallRecord) *Report {
	r := &Report{GeneratedAt: time.Now()}

	byAgent := map[string]*AgentSummary{}
	byModel := map[string]*ModelSummary{}

	for _, rec := range records {
		r.TotalCalls++
		r.InputTokens += rec.InputTokens
		r.OutputTokens += rec.OutputTokens
		r.CacheReadTokens += rec.CacheReadTokens
		r.TotalTokens += rec.InputTokens + rec.OutputTokens
		r.TotalTimeMs += rec.DurationMs

		// Per-agent.
		as := byAgent[rec.Agent]
		if as == nil {
			as = &AgentSummary{Agent: rec.Agent, Model: rec.Model}
			byAgent[rec.Agent] = as
		}
		as.TotalCalls++
		as.InputTokens += rec.InputTokens
		as.OutputTokens += rec.OutputTokens
		as.CacheReadTokens += rec.CacheReadTokens
		as.TotalTokens += rec.InputTokens + rec.OutputTokens
		as.TotalTimeMs += rec.DurationMs

		// Per-model.
		ms := byModel[rec.Model]
		if ms == nil {
			ms = &ModelSummary{Model: rec.Model}
			byModel[rec.Model] = ms
		}
		ms.TotalCalls++
		ms.InputTokens += rec.InputTokens
		ms.OutputTokens += rec.OutputTokens
		ms.CacheReadTokens += rec.CacheReadTokens
		ms.TotalTokens += rec.InputTokens + rec.OutputTokens
	}

	// Compute averages and costs.
	for _, as := range byAgent {
		if as.TotalCalls > 0 {
			as.AvgLatencyMs = as.TotalTimeMs / as.TotalCalls
		}
		as.EstCostUSD = estimateCost(as.Model, as.InputTokens, as.OutputTokens, as.CacheReadTokens)
		r.Agents = append(r.Agents, *as)
	}
	sort.Slice(r.Agents, func(i, j int) bool { return r.Agents[i].Agent < r.Agents[j].Agent })

	for _, ms := range byModel {
		ms.EstCostUSD = estimateCost(ms.Model, ms.InputTokens, ms.OutputTokens, ms.CacheReadTokens)
		r.Models = append(r.Models, *ms)
	}
	sort.Slice(r.Models, func(i, j int) bool { return r.Models[i].Model < r.Models[j].Model })

	for _, ms := range r.Models {
		r.EstCostUSD += ms.EstCostUSD
	}

	return r
}

// EstimateCost returns an approximate USD cost based on model pricing.
// This is the public API; the internal estimateCost function is used by the report generator.
func EstimateCost(model string, inputTokens, outputTokens, cacheReadTokens int64) float64 {
	return estimateCost(model, inputTokens, outputTokens, cacheReadTokens)
}

// estimateCost returns an approximate USD cost based on model pricing.
// Prices are per million tokens. Cache-read tokens are billed at 10% of
// the normal input price (Anthropic prompt caching discount).
func estimateCost(model string, inputTokens, outputTokens, cacheReadTokens int64) float64 {
	inPrice, outPrice := modelPricing(model)
	if cacheReadTokens > 0 && cacheReadTokens <= inputTokens {
		uncached := inputTokens - cacheReadTokens
		return float64(uncached)/1e6*inPrice +
			float64(cacheReadTokens)/1e6*inPrice*0.1 +
			float64(outputTokens)/1e6*outPrice
	}
	return float64(inputTokens)/1e6*inPrice + float64(outputTokens)/1e6*outPrice
}

// modelPricing returns (input $/M, output $/M) for known models.
// Falls back to a conservative estimate for unknown models.
func modelPricing(model string) (float64, float64) {
	// Pricing as of 2025. provider/model-id format.
	switch {
	// Anthropic
	case strings.Contains(model, "claude-opus"):
		return 15.0, 75.0
	case strings.Contains(model, "claude-sonnet-4"):
		return 3.0, 15.0
	case strings.Contains(model, "claude-sonnet"):
		return 3.0, 15.0
	case strings.Contains(model, "claude-haiku"):
		return 0.25, 1.25
	// OpenAI
	case strings.Contains(model, "gpt-4o"):
		return 2.5, 10.0
	case strings.Contains(model, "gpt-4-turbo"), strings.Contains(model, "gpt-4-1"):
		return 10.0, 30.0
	case strings.Contains(model, "gpt-4"):
		return 30.0, 60.0
	case strings.Contains(model, "o1"):
		return 15.0, 60.0
	case strings.Contains(model, "o3"):
		return 10.0, 40.0
	// Google
	case strings.Contains(model, "gemini-2.5-pro"):
		return 1.25, 10.0
	case strings.Contains(model, "gemini-2.5-flash"):
		return 0.15, 0.6
	case strings.Contains(model, "gemini-2.0-flash"):
		return 0.1, 0.4
	case strings.Contains(model, "gemini-1.5-pro"):
		return 1.25, 5.0
	case strings.Contains(model, "gemini-1.5-flash"):
		return 0.075, 0.3
	default:
		return 3.0, 15.0 // conservative default
	}
}

