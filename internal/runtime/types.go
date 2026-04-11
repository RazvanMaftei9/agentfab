package runtime

import "time"

type Endpoint struct {
	Address string
	Local   bool
}

type AgentDefinition struct {
	Name                 string            `yaml:"name" json:"name"`
	Purpose              string            `yaml:"purpose" json:"purpose"`
	Capabilities         []string          `yaml:"capabilities" json:"capabilities"`
	Model                string            `yaml:"model" json:"model"`
	SpecialKnowledgeFile string            `yaml:"special_knowledge_file,omitempty" json:"special_knowledge_file,omitempty"`
	EscalationTarget     string            `yaml:"escalation_target,omitempty" json:"escalation_target,omitempty"`
	Budget               *Budget           `yaml:"budget,omitempty" json:"budget,omitempty"`
	Tools                []ToolConfig      `yaml:"tools,omitempty" json:"tools,omitempty"`
	MaxConcurrentTasks   int               `yaml:"max_concurrent_tasks,omitempty" json:"max_concurrent_tasks,omitempty"` // 0 = unlimited
	RequiredNodeLabels   map[string]string `yaml:"required_node_labels,omitempty" json:"required_node_labels,omitempty"`
	MaxKnowledgeNodes    int               `yaml:"max_knowledge_nodes,omitempty" json:"max_knowledge_nodes,omitempty"` // 0 = default (200)
	TaskTimeout          time.Duration     `yaml:"task_timeout,omitempty" json:"task_timeout,omitempty"`               // per-task timeout; 0 = no timeout
	ReviewGuidelines     string            `yaml:"review_guidelines,omitempty" json:"review_guidelines,omitempty"`
	ReviewPrompt         string            `yaml:"review_prompt,omitempty" json:"review_prompt,omitempty"`
	Verify               *VerifyConfig     `yaml:"verify,omitempty" json:"verify,omitempty"`
	ColdRetentionDays    int               `yaml:"cold_storage_retention_days,omitempty" json:"cold_storage_retention_days,omitempty"` // days to retain cold nodes; 0 = default (1095)
	CurationThreshold    int               `yaml:"curation_threshold,omitempty" json:"curation_threshold,omitempty"`                   // min nodes before idle curation triggers; 0 = default (50)
	ModelTiers           []string          `yaml:"model_tiers,omitempty" json:"model_tiers,omitempty"`                                 // ordered list of models for adaptive routing [cheap → expensive]
	ShellOnly            bool              `yaml:"shell_only,omitempty" json:"shell_only,omitempty"`                                   // when true, skip file-artifact instructions; agent works only via shell tools
	Address              string            `yaml:"address,omitempty" json:"address,omitempty"`                                         // gRPC address for remote agents; empty = local
}

// ToolConfig declares a tool available to an agent.
// Mode: "live" (LLM-callable), "post_process" (after task), "both", or empty (heuristic).
type ToolConfig struct {
	Name           string               `yaml:"name" json:"name"`
	Instructions   string               `yaml:"instructions" json:"instructions"`
	Command        string               `yaml:"command,omitempty" json:"command,omitempty"`
	Config         map[string]string    `yaml:"config,omitempty" json:"config,omitempty"`
	Parameters     map[string]ToolParam `yaml:"parameters,omitempty" json:"parameters,omitempty"`
	Mode           string               `yaml:"mode,omitempty" json:"mode,omitempty"`                       // "live", "post_process", "both"; empty = heuristic
	PassthroughEnv []string             `yaml:"passthrough_env,omitempty" json:"passthrough_env,omitempty"` // Host env vars to pass through to the sandbox, bypassing the blocklist.
}

func (tc ToolConfig) IsLive() bool {
	switch tc.Mode {
	case "live", "both":
		return true
	case "post_process":
		return false
	default:
		return len(tc.Parameters) > 0
	}
}

func (tc ToolConfig) IsPostProcess() bool {
	switch tc.Mode {
	case "post_process", "both":
		return true
	case "live":
		return false
	default:
		return tc.Command != "" && len(tc.Parameters) == 0
	}
}

type ToolParam struct {
	Type        string `yaml:"type" json:"type"` // "string", "integer", "boolean", "number"
	Description string `yaml:"description" json:"description"`
	Required    bool   `yaml:"required,omitempty" json:"required,omitempty"`
}

type Budget struct {
	MaxInputTokens  int64   `yaml:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64   `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int64   `yaml:"max_total_tokens,omitempty" json:"max_total_tokens,omitempty"`
	MaxCostUSD      float64 `yaml:"max_cost_usd,omitempty" json:"max_cost_usd,omitempty"`
}

// VerifyConfig defines an automatic verification command that runs after the
// agent's tool loop ends. If the command fails, errors are injected into the
// conversation and the agent gets bonus iterations to fix them.
type VerifyConfig struct {
	Command         string        `yaml:"command" json:"command"`
	MaxRetries      int           `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
	BonusIterations int           `yaml:"bonus_iterations,omitempty" json:"bonus_iterations,omitempty"`
	Timeout         time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"` // command timeout; 0 = default (120s)
}

// LLMCallRecord captures a single LLM API call for metering.
type LLMCallRecord struct {
	AgentName       string
	RequestID       string
	TaskID          string
	Model           string
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64 // Tokens served from prompt cache (subset of InputTokens).
	Duration        time.Duration
	Timestamp       time.Time
	FinishReason    string // e.g. "stop", "length", "tool_use"
}

// UsageSummary aggregates token usage.
type UsageSummary struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64 // Tokens served from prompt cache (subset of InputTokens).
	TotalTokens     int64
	TotalCalls      int64
	TotalTime       time.Duration
}
