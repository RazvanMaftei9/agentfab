package bench

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/razvanmaftei/agentfab/internal/conductor"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/local"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

func (e *KnowledgeExperiment) Run(ctx context.Context) (*KnowledgeResult, error) {
	result := &KnowledgeResult{}

	slog.Info("knowledge experiment: phase 1 - accumulation", "scenarios", len(e.Scenarios))
	for i, s := range e.Scenarios {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		slog.Info("knowledge experiment: accumulation run", "scenario", s.Name, "index", i+1)
		origDir := e.Runner.OutputDir
		e.Runner.OutputDir = e.SharedDir

		results, err := e.runSingleShared(ctx, s, i)
		e.Runner.OutputDir = origDir

		if err != nil {
			slog.Warn("knowledge experiment: accumulation run failed", "error", err)
		}
		result.AccumulationRuns = append(result.AccumulationRuns, results...)
	}

	slog.Info("knowledge experiment: phase 2 - pre-curation snapshot")
	result.PreCuration = e.snapshotGraphs(ctx)

	slog.Info("knowledge experiment: phase 3 - curation")
	curationResults, err := e.forceCuration(ctx)
	if err != nil {
		slog.Warn("knowledge experiment: curation failed", "error", err)
	}
	result.CurationResults = curationResults

	slog.Info("knowledge experiment: phase 4 - post-curation snapshot")
	result.PostCuration = e.snapshotGraphs(ctx)

	slog.Info("knowledge experiment: phase 5 - post-curation runs")
	for i, s := range e.Scenarios {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}

		slog.Info("knowledge experiment: post-curation run", "scenario", s.Name, "index", i+1)
		origDir := e.Runner.OutputDir
		e.Runner.OutputDir = e.SharedDir

		results, err := e.runSingleShared(ctx, s, i+len(e.Scenarios))
		e.Runner.OutputDir = origDir

		if err != nil {
			slog.Warn("knowledge experiment: post-curation run failed", "error", err)
		}
		result.PostRuns = append(result.PostRuns, results...)
	}

	slog.Info("knowledge experiment: phase 6 - computing deltas")
	e.computeDeltas(result)

	return result, nil
}

func (e *KnowledgeExperiment) runSingleShared(ctx context.Context, s Scenario, idx int) ([]BenchResult, error) {
	singleRun := s
	singleRun.Runs = 1
	if len(singleRun.Configs) > 1 {
		singleRun.Configs = singleRun.Configs[:1]
	}

	// Create a conductor using the shared data dir.
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

	def := e.Runner.BaseFabricDef
	c, err := conductor.New(def, e.SharedDir, e.Runner.Factory, bus)
	if err != nil {
		return nil, fmt.Errorf("create conductor: %w", err)
	}

	start := time.Now()
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("start conductor: %w", err)
	}

	_, reqErr := c.HandleRequest(ctx, s.Request)
	duration := time.Since(start)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	c.Shutdown(shutCtx)

	bus.Close()
	<-doneCh

	// Build result.
	allUsage := c.Meter.AllAgentUsage(ctx)
	var totalIn, totalOut int64
	for _, usage := range allUsage {
		totalIn += usage.InputTokens
		totalOut += usage.OutputTokens
	}

	var completed, failed int
	for _, evt := range taskEvents {
		if evt.Type == event.TaskComplete {
			completed++
		} else {
			failed++
		}
	}

	result := BenchResult{
		Scenario:       s.Name,
		Config:         "shared",
		RunIndex:       idx,
		InputTokens:    totalIn,
		OutputTokens:   totalOut,
		Duration:       duration,
		TasksCompleted: completed,
		TasksFailed:    failed,
	}

	if reqErr != nil {
		result.Error = reqErr.Error()
	}

	return []BenchResult{result}, nil
}

// snapshotGraphs captures the knowledge graph state for all agents.
func (e *KnowledgeExperiment) snapshotGraphs(ctx context.Context) []GraphSnapshot {
	var snapshots []GraphSnapshot
	for _, def := range e.Runner.BaseFabricDef.Agents {
		if def.Name == "conductor" {
			continue
		}

		agentStorage := local.NewStorage(e.SharedDir, def.Name)
		agentGraph, err := knowledge.LoadFromTier(ctx, agentStorage, runtime.TierAgent)
		if err != nil || agentGraph == nil {
			snapshots = append(snapshots, GraphSnapshot{Agent: def.Name})
			continue
		}

		nodes, edges, decisions := knowledge.GraphStats(agentGraph)

		// Load shared graph for context size estimation.
		sharedStorage := local.NewStorage(e.SharedDir, "conductor")
		sharedGraph, _ := knowledge.Load(ctx, sharedStorage)

		// Estimate context tokens for reference tasks.
		totalContextTokens := 0
		for _, taskDesc := range e.RefTasks {
			totalContextTokens += knowledge.MeasureContextSize(agentGraph, sharedGraph, def.Name, taskDesc)
		}
		avgContextTokens := 0
		if len(e.RefTasks) > 0 {
			avgContextTokens = totalContextTokens / len(e.RefTasks)
		}

		snapshots = append(snapshots, GraphSnapshot{
			Agent:         def.Name,
			Nodes:         nodes,
			Edges:         edges,
			DecisionNodes: decisions,
			ContextTokens: avgContextTokens,
		})
	}
	return snapshots
}

// forceCuration creates a temporary conductor and forces curation for all agents.
func (e *KnowledgeExperiment) forceCuration(ctx context.Context) ([]conductor.CurationResult, error) {
	bus := event.NewBus()
	defer bus.Close()

	def := e.Runner.BaseFabricDef
	c, err := conductor.New(def, e.SharedDir, e.Runner.Factory, bus)
	if err != nil {
		return nil, fmt.Errorf("create conductor for curation: %w", err)
	}

	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("start conductor for curation: %w", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutCancel()
		c.Shutdown(shutCtx)
	}()

	return c.ForceCuration(ctx)
}

// computeDeltas calculates Phase 6 comparison metrics.
func (e *KnowledgeExperiment) computeDeltas(result *KnowledgeResult) {
	// Cost savings: average cost of accumulation runs vs post-curation runs.
	preCost := avgCost(result.AccumulationRuns)
	postCost := avgCost(result.PostRuns)
	if preCost > 0 {
		result.CostSavingsPct = (preCost - postCost) / preCost * 100
	}

	// Context reduction: average context tokens pre vs post curation.
	preCtx := avgContextTokens(result.PreCuration)
	postCtx := avgContextTokens(result.PostCuration)
	if preCtx > 0 {
		result.ContextReductionPct = float64(preCtx-postCtx) / float64(preCtx) * 100
	}

	// Quality delta: assertion pass rate difference.
	preQuality := assertPassRate(result.AccumulationRuns)
	postQuality := assertPassRate(result.PostRuns)
	result.QualityDelta = postQuality - preQuality
}

func avgCost(runs []BenchResult) float64 {
	if len(runs) == 0 {
		return 0
	}
	total := 0.0
	for _, r := range runs {
		total += r.EstCostUSD
	}
	return total / float64(len(runs))
}

func avgContextTokens(snapshots []GraphSnapshot) int {
	if len(snapshots) == 0 {
		return 0
	}
	total := 0
	for _, s := range snapshots {
		total += s.ContextTokens
	}
	return total / len(snapshots)
}

func assertPassRate(runs []BenchResult) float64 {
	totalPassed, totalFailed := 0, 0
	for _, r := range runs {
		totalPassed += r.AssertsPassed
		totalFailed += r.AssertsFailed
	}
	total := totalPassed + totalFailed
	if total == 0 {
		return 0
	}
	return float64(totalPassed) / float64(total) * 100
}

// GenerateKnowledgeReport produces a Markdown report for a knowledge experiment.
func GenerateKnowledgeReport(result *KnowledgeResult) string {
	var sb fmt.Stringer = &knowledgeReportBuilder{result: result}
	return sb.(fmt.Stringer).String()
}

type knowledgeReportBuilder struct {
	result *KnowledgeResult
}

func (b *knowledgeReportBuilder) String() string {
	r := b.result
	s := "# Knowledge Curation Evaluation\n\n"

	// Table 1: Curation Summary
	s += "## Curation Summary\n\n"
	s += "| Agent | Nodes Before | After | Reduction | Cold Moved | Cold Purged |\n"
	s += "|-------|-------------|-------|-----------|------------|------------|\n"
	for _, cr := range r.CurationResults {
		reduction := 0.0
		if cr.NodesBefore > 0 {
			reduction = float64(cr.NodesBefore-cr.NodesAfter) / float64(cr.NodesBefore) * 100
		}
		s += fmt.Sprintf("| %s | %d | %d | %.0f%% | %d | %d |\n",
			cr.Agent, cr.NodesBefore, cr.NodesAfter, reduction, cr.ColdMoved, cr.ColdPurged)
	}
	s += "\n"

	// Table 2: Context Injection Size
	s += "## Context Injection Size\n\n"
	s += "| Agent | Tokens Before | After | Reduction |\n"
	s += "|-------|--------------|-------|----------|\n"
	preMap := make(map[string]GraphSnapshot)
	for _, snap := range r.PreCuration {
		preMap[snap.Agent] = snap
	}
	for _, post := range r.PostCuration {
		pre := preMap[post.Agent]
		reduction := 0.0
		if pre.ContextTokens > 0 {
			reduction = float64(pre.ContextTokens-post.ContextTokens) / float64(pre.ContextTokens) * 100
		}
		s += fmt.Sprintf("| %s | %d | %d | %.0f%% |\n",
			post.Agent, pre.ContextTokens, post.ContextTokens, reduction)
	}
	s += "\n"

	// Table 3: Cost and Quality
	s += "## Cost and Quality Comparison\n\n"
	s += "| Phase | Avg Cost/Task | Avg Input Tokens | Assertion Rate |\n"
	s += "|-------|---------------|------------------|---------------|\n"

	preCost := avgCost(r.AccumulationRuns)
	postCost := avgCost(r.PostRuns)
	preTokens := avgInputTokens(r.AccumulationRuns)
	postTokens := avgInputTokens(r.PostRuns)
	preAssert := assertPassRate(r.AccumulationRuns)
	postAssert := assertPassRate(r.PostRuns)

	s += fmt.Sprintf("| Pre-Curation | $%.2f | %d | %.0f%% |\n", preCost, preTokens, preAssert)
	s += fmt.Sprintf("| Post-Curation | $%.2f | %d | %.0f%% |\n", postCost, postTokens, postAssert)

	costDelta := ""
	if preCost > 0 {
		costDelta = fmt.Sprintf("%.0f%%", (preCost-postCost)/preCost*100)
	}
	tokenDelta := ""
	if preTokens > 0 {
		tokenDelta = fmt.Sprintf("%.0f%%", float64(preTokens-postTokens)/float64(preTokens)*100)
	}
	s += fmt.Sprintf("| Delta | %s | %s | %+.0f%% |\n", costDelta, tokenDelta, postAssert-preAssert)
	s += "\n"

	return s
}

func avgInputTokens(runs []BenchResult) int64 {
	if len(runs) == 0 {
		return 0
	}
	var total int64
	for _, r := range runs {
		total += r.InputTokens
	}
	return total / int64(len(runs))
}

