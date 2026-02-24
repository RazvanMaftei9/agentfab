package bench

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"

	"gopkg.in/yaml.v3"

	"github.com/razvanmaftei/agentfab/internal/conductor"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/metrics"
	"github.com/razvanmaftei/agentfab/internal/routing"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type Runner struct {
	BaseFabricDef *config.FabricDef
	Factory       conductor.ModelFactory
	OutputDir     string
	Debug         bool
}

func (r *Runner) RunScenario(ctx context.Context, s Scenario) ([]BenchResult, error) {
	runs := s.Runs
	if runs <= 0 {
		runs = 3
	}

	var results []BenchResult
	for _, cfg := range s.Configs {
		for i := 0; i < runs; i++ {
			select {
			case <-ctx.Done():
				return results, ctx.Err()
			default:
			}

			slog.Info("bench: starting run",
				"scenario", s.Name, "config", cfg.Name, "run", i+1)

			result, err := r.runSingle(ctx, s, cfg, i)
			if err != nil {
				result.Error = err.Error()
				slog.Warn("bench: run failed",
					"scenario", s.Name, "config", cfg.Name, "run", i+1, "error", err)
			}
			results = append(results, result)
		}
	}
	return results, nil
}

func (r *Runner) runSingle(ctx context.Context, s Scenario, cfg RunConfig, idx int) (BenchResult, error) {
	result := BenchResult{
		Scenario: s.Name,
		Config:   cfg.Name,
		RunIndex: idx,
	}

	def, err := r.applyOverrides(s, cfg)
	if err != nil {
		return result, fmt.Errorf("apply overrides: %w", err)
	}

	runDir := filepath.Join(r.OutputDir, s.Name, cfg.Name, fmt.Sprintf("run-%d", idx))
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return result, fmt.Errorf("create run dir: %w", err)
	}

	bus := event.NewBus()
	var taskEvents []event.Event
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		for evt := range bus {
			switch evt.Type {
			case event.TaskComplete, event.TaskFailed:
				taskEvents = append(taskEvents, evt)
			}
		}
	}()

	factory := r.Factory
	var router *routing.AdaptiveRouter
	if cfg.Adaptive {
		tiers := make(map[string][]string)
		for _, a := range def.Agents {
			if len(a.ModelTiers) > 0 {
				tiers[a.Name] = a.ModelTiers
			}
		}
		if len(tiers) > 0 {
			router = routing.NewAdaptiveRouter(r.Factory, tiers)
			factory = router.ModelFactory()
		}
	}

	c, err := conductor.New(def, runDir, factory, bus)
	if err != nil {
		return result, fmt.Errorf("create conductor: %w", err)
	}

	start := time.Now()
	if err := c.Start(ctx); err != nil {
		return result, fmt.Errorf("start conductor: %w", err)
	}

	_, reqErr := c.HandleRequest(ctx, s.Request)
	result.Duration = time.Since(start)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	c.Shutdown(shutCtx)

	bus.Close()
	<-doneCh

	allUsage := c.Meter.AllAgentUsage(ctx)
	allRecords := c.Meter.AllRecords(ctx)

	var totalIn, totalOut, totalCache, totalCalls int64
	for agent, usage := range allUsage {
		totalIn += usage.InputTokens
		totalOut += usage.OutputTokens
		totalCache += usage.CacheReadTokens
		totalCalls += usage.TotalCalls

		agentModel := ""
		for _, a := range def.Agents {
			if a.Name == agent {
				agentModel = a.Model
				break
			}
		}

		am := AgentMetric{
			Agent:        agent,
			Model:        agentModel,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			Calls:        usage.TotalCalls,
			EstCostUSD:   metrics.EstimateCost(agentModel, usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens),
		}
		result.AgentMetrics = append(result.AgentMetrics, am)
	}

	result.InputTokens = totalIn
	result.OutputTokens = totalOut
	result.CacheTokens = totalCache
	result.TotalCalls = totalCalls

	for _, rec := range allRecords {
		result.EstCostUSD += metrics.EstimateCost(rec.Model, rec.InputTokens, rec.OutputTokens, rec.CacheReadTokens)
	}

	for _, evt := range taskEvents {
		switch evt.Type {
		case event.TaskComplete:
			result.TasksCompleted++
		case event.TaskFailed:
			result.TasksFailed++
		}
	}

	artifactsDir := filepath.Join(runDir, "shared", "artifacts")
	passed, failed := RunAssertions(s.Assertions, artifactsDir, runDir)
	result.AssertsPassed = passed
	result.AssertsFailed = failed
	result.BuildPassed = allBuildsPassed(s.Assertions, artifactsDir, runDir)

	if router != nil {
		result.TierStats = router.Report()
	}

	if len(allRecords) > 0 {
		result.RequestID = allRecords[0].RequestID
	}

	if reqErr != nil {
		return result, reqErr
	}

	return result, nil
}

func (r *Runner) applyOverrides(s Scenario, cfg RunConfig) (*config.FabricDef, error) {
	def, err := deepCopyDef(r.BaseFabricDef)
	if err != nil {
		return nil, err
	}

	if cfg.SingleAgent {
		var kept []runtime.AgentDefinition
		for _, a := range def.Agents {
			if a.Name == "conductor" || a.Name == "developer" {
				kept = append(kept, a)
			}
		}
		def.Agents = kept
	}

	for i := range def.Agents {
		a := &def.Agents[i]
		if model, ok := cfg.ModelOverrides[a.Name]; ok {
			a.Model = model
		}
		if cfg.DisableVerify {
			a.Verify = nil
		}
		if cfg.DisableKnowledge {
			a.MaxKnowledgeNodes = 0
		}
		if s.VerifyCommand != "" && a.Verify != nil {
			if strings.EqualFold(s.VerifyCommand, "none") {
				a.Verify = nil
			} else {
				a.Verify.Command = s.VerifyCommand
			}
		}
	}

	return &def, nil
}

func loadScenarioFromFile(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Runs <= 0 {
		s.Runs = 3
	}
	return &s, nil
}

func loadAllScenariosFromDir(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var scenarios []Scenario
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		s, err := loadScenarioFromFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		scenarios = append(scenarios, *s)
	}
	return scenarios, nil
}

func allBuildsPassed(assertions []Assertion, artifactsDir, runDir string) bool {
	for _, a := range assertions {
		if a.Type == "build_passes" {
			if !checkBuildPasses(a, artifactsDir, runDir) {
				return false
			}
		}
	}
	return true
}

var providerEnvVars = map[string]string{
	"openai":    "OPENAI_API_KEY",
	"anthropic": "ANTHROPIC_API_KEY",
	"google":    "GOOGLE_API_KEY",
}

// ValidateAPIKeys returns missing API keys needed by the scenarios' models.
func ValidateAPIKeys(def *config.FabricDef, scenarios []Scenario) []string {
	needed := make(map[string][]string) // env var -> list of "scenario/config/agent" references

	for _, a := range def.Agents {
		provider, _, err := parseProvider(a.Model)
		if err != nil {
			continue
		}
		if envVar, ok := providerEnvVars[provider]; ok {
			needed[envVar] = append(needed[envVar], fmt.Sprintf("default/%s", a.Name))
		}
	}

	for _, s := range scenarios {
		for _, cfg := range s.Configs {
			for agent, modelID := range cfg.ModelOverrides {
				provider, _, err := parseProvider(modelID)
				if err != nil {
					continue
				}
				if envVar, ok := providerEnvVars[provider]; ok {
					needed[envVar] = append(needed[envVar], fmt.Sprintf("%s/%s/%s", s.Name, cfg.Name, agent))
				}
			}
		}
	}

	var missing []string
	for envVar, refs := range needed {
		if os.Getenv(envVar) == "" {
			missing = append(missing, fmt.Sprintf("%s not set (needed by %d model references, e.g. %s)", envVar, len(refs), refs[0]))
		}
	}
	return missing
}

func deepCopyDef(src *config.FabricDef) (config.FabricDef, error) {
	data, err := json.Marshal(src)
	if err != nil {
		return config.FabricDef{}, err
	}
	var dst config.FabricDef
	err = json.Unmarshal(data, &dst)
	return dst, err
}

func parseProvider(modelID string) (string, string, error) {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid model ID: %s", modelID)
	}
	return parts[0], parts[1], nil
}
