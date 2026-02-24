package bench

import (
	"time"

	"github.com/razvanmaftei/agentfab/internal/routing"
)

type Scenario struct {
	Name           string      `yaml:"name"`
	Description    string      `yaml:"description"`
	Request        string      `yaml:"request"`
	Configs        []RunConfig `yaml:"configs"`
	Assertions     []Assertion `yaml:"assertions"`
	Runs           int         `yaml:"runs"`                    // default 3
	Category       string      `yaml:"category,omitempty"`      // "ablation", "competitive", "complexity"
	Complexity     string      `yaml:"complexity,omitempty"`    // "simple", "medium", "complex"
	TestCommand    string      `yaml:"test_command,omitempty"`  // e.g., "pytest test.py"
	VerifyCommand  string      `yaml:"verify_command,omitempty"` // override agent verify; "none" to disable
}

type RunConfig struct {
	Name             string            `yaml:"name"`
	ModelOverrides   map[string]string `yaml:"model_overrides,omitempty"`
	DisableVerify    bool              `yaml:"disable_verify,omitempty"`
	Adaptive         bool              `yaml:"adaptive,omitempty"`
	SingleAgent      bool              `yaml:"single_agent,omitempty"`      // conductor+developer only, no decomposition
	DisableKnowledge bool              `yaml:"disable_knowledge,omitempty"` // no knowledge graph injection
}

type Assertion struct {
	Type    string `yaml:"type"`              // file_exists | build_passes | pattern_match | file_count_min
	Path    string `yaml:"path,omitempty"`    // glob pattern for file assertions
	Pattern string `yaml:"pattern,omitempty"` // regex for pattern_match
	Command string `yaml:"command,omitempty"` // shell command for build_passes
	Min     int    `yaml:"min,omitempty"`     // minimum count for file_count_min
}

type BenchResult struct {
	Scenario string // scenario name
	Config   string // config name
	RunIndex int
	RequestID string

	InputTokens  int64
	OutputTokens int64
	CacheTokens  int64
	TotalCalls   int64
	EstCostUSD   float64

	Duration time.Duration

	TasksCompleted int
	TasksFailed    int
	TasksEscalated int
	BuildPassed    bool
	AssertsPassed  int
	AssertsFailed  int

	AgentMetrics []AgentMetric
	TierStats    []routing.TierStat `json:"tier_stats,omitempty"`

	Error string
}

type AgentMetric struct {
	Agent        string
	Model        string
	InputTokens  int64
	OutputTokens int64
	Calls        int64
	EstCostUSD   float64
}

func LoadScenario(path string) (*Scenario, error) {
	return loadScenarioFromFile(path)
}

func LoadAllScenarios(dir string) ([]Scenario, error) {
	return loadAllScenariosFromDir(dir)
}
