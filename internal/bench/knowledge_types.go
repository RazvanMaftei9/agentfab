package bench

import "github.com/razvanmaftei/agentfab/internal/conductor"

// KnowledgeExperiment defines a knowledge curation evaluation experiment.
type KnowledgeExperiment struct {
	Runner    *Runner
	Scenarios []Scenario // task sequence for accumulation
	RefTasks  []string   // reference task descriptions for context size measurement
	SharedDir string     // single data dir for the experiment (knowledge accumulates)
}

// KnowledgeResult captures the full outcome of a knowledge curation experiment.
type KnowledgeResult struct {
	// Phase 1: accumulation runs
	AccumulationRuns []BenchResult

	// Phase 2 + 4: graph snapshots before and after curation
	PreCuration  []GraphSnapshot
	PostCuration []GraphSnapshot

	// Phase 3: curation outcomes
	CurationResults []conductor.CurationResult

	// Phase 5: post-curation runs
	PostRuns []BenchResult

	// Phase 6: computed deltas
	CostSavingsPct      float64
	ContextReductionPct float64
	QualityDelta        float64 // assertion pass rate difference
}

// GraphSnapshot captures the state of an agent's knowledge graph at a point in time.
type GraphSnapshot struct {
	Agent         string
	Nodes         int
	Edges         int
	DecisionNodes int
	ContextTokens int // estimated injection size for reference tasks
}
