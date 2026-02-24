package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// GenerateReport produces a Markdown report from benchmark results.
func GenerateReport(results []BenchResult) string {
	if len(results) == 0 {
		return "No benchmark results.\n"
	}

	var sb strings.Builder
	sb.WriteString("# Benchmark Results\n\n")

	byScenario := groupByScenario(results)
	scenarios := sortedKeys(byScenario)

	for _, scenario := range scenarios {
		scenarioResults := byScenario[scenario]
		sb.WriteString(fmt.Sprintf("## %s\n\n", scenario))

		byConfig := groupByConfig(scenarioResults)
		configs := sortedKeys(byConfig)

		sb.WriteString("| Config | Cost (USD) | Duration | Tokens (In/Out) | Build | Tasks OK/Fail | Asserts |\n")
		sb.WriteString("|--------|------------|----------|-----------------|-------|---------------|--------|\n")

		for _, cfg := range configs {
			cfgResults := byConfig[cfg]
			agg := aggregateResults(cfgResults)
			sb.WriteString(fmt.Sprintf("| %s | $%.2f | %s | %s/%s | %s | %d/%d | %d/%d |\n",
				cfg,
				agg.MeanCost,
				formatDuration(agg.MeanDuration),
				formatTokens(agg.MeanInputTokens),
				formatTokens(agg.MeanOutputTokens),
				boolPass(agg.BuildPassRate >= 0.5),
				agg.MeanTasksCompleted,
				agg.MeanTasksFailed,
				agg.MeanAssertsPassed,
				agg.MeanAssertsFailed,
			))
		}
		sb.WriteString("\n")

		sb.WriteString("### Statistical Summary (mean ± stddev)\n\n")
		sb.WriteString("| Config | Runs | Cost | Duration | Build Rate | Assert Rate |\n")
		sb.WriteString("|--------|------|------|----------|------------|------------|\n")

		for _, cfg := range configs {
			cfgResults := byConfig[cfg]
			agg := aggregateResults(cfgResults)
			totalAsserts := agg.MeanAssertsPassed + agg.MeanAssertsFailed
			assertRate := float64(0)
			if totalAsserts > 0 {
				assertRate = float64(agg.MeanAssertsPassed) / float64(totalAsserts) * 100
			}
			sb.WriteString(fmt.Sprintf("| %s | %d | $%.2f ± $%.2f | %s ± %s | %.0f%% | %.0f%% |\n",
				cfg,
				len(cfgResults),
				agg.MeanCost, agg.StddevCost,
				formatDuration(agg.MeanDuration), formatDuration(agg.StddevDuration),
				agg.BuildPassRate*100,
				assertRate,
			))
		}
		sb.WriteString("\n")

		sb.WriteString("### Per-Agent Breakdown\n\n")
		sb.WriteString("| Config | Agent | Model | Tokens (In/Out) | Calls | Cost |\n")
		sb.WriteString("|--------|-------|-------|-----------------|-------|------|\n")

		for _, cfg := range configs {
			cfgResults := byConfig[cfg]
			agentAgg := aggregateAgentMetrics(cfgResults)
			agents := sortedKeys(agentAgg)
			for _, agent := range agents {
				am := agentAgg[agent]
				sb.WriteString(fmt.Sprintf("| %s | %s | %s | %s/%s | %d | $%.2f |\n",
					cfg, agent, am.Model,
					formatTokens(am.InputTokens),
					formatTokens(am.OutputTokens),
					am.Calls,
					am.EstCostUSD,
				))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func WriteJSON(results []BenchResult, path string) error {
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func ReadJSON(path string) ([]BenchResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var results []BenchResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GenerateComparisonReport produces a statistical comparison between two result sets.
func GenerateComparisonReport(baseline, treatment []BenchResult, baselineName, treatmentName string) string {
	if len(baseline) == 0 || len(treatment) == 0 {
		return "Insufficient data for comparison.\n"
	}

	var sb strings.Builder
	sb.WriteString("# Comparison Report\n\n")
	sb.WriteString(fmt.Sprintf("Baseline: **%s** (%d runs)  \n", baselineName, len(baseline)))
	sb.WriteString(fmt.Sprintf("Treatment: **%s** (%d runs)\n\n", treatmentName, len(treatment)))

	sb.WriteString("## Overall\n\n")
	sb.WriteString(ComparisonSummary(baseline, treatment, baselineName, treatmentName))
	sb.WriteString("\n")

	bCosts := extractCosts(baseline)
	tCosts := extractCosts(treatment)
	d := CohenD(bCosts, tCosts)
	lo, hi := BootstrapCI(bCosts, tCosts, 10000, 0.05)
	sb.WriteString("### Effect Size\n\n")
	sb.WriteString(fmt.Sprintf("- Cohen's d (cost): %.2f\n", d))
	sb.WriteString(fmt.Sprintf("- Bootstrap 95%% CI for cost delta: [%.1f%%, %.1f%%]\n\n", lo, hi))

	bByScenario := groupByScenario(baseline)
	tByScenario := groupByScenario(treatment)
	allScenarios := make(map[string]bool)
	for k := range bByScenario {
		allScenarios[k] = true
	}
	for k := range tByScenario {
		allScenarios[k] = true
	}

	if len(allScenarios) > 1 {
		sb.WriteString("## Per-Scenario Comparison\n\n")
		for _, scenario := range sortedKeys(allScenarios) {
			bSet, bOk := bByScenario[scenario]
			tSet, tOk := tByScenario[scenario]
			if !bOk || !tOk {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s\n\n", scenario))
			sb.WriteString(ComparisonSummary(bSet, tSet, baselineName, treatmentName))
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// GenerateAdaptiveReport produces a report on adaptive routing tier escalation.
func GenerateAdaptiveReport(results []BenchResult) string {
	var sb strings.Builder
	sb.WriteString("# Adaptive Routing Report\n\n")

	type tierKey struct {
		Agent string
		Model string
		Tier  int
	}
	agg := make(map[tierKey]*[3]int) // [success, failure, total]

	for _, r := range results {
		for _, ts := range r.TierStats {
			k := tierKey{ts.Agent, ts.Model, ts.Tier}
			if _, ok := agg[k]; !ok {
				agg[k] = &[3]int{}
			}
			agg[k][0] += ts.Success
			agg[k][1] += ts.Failure
			agg[k][2] += ts.Total
		}
	}

	if len(agg) == 0 {
		sb.WriteString("No adaptive routing data available.\n")
		return sb.String()
	}

	sb.WriteString("| Agent | Model | Tier | Success | Failure | Total | Rate |\n")
	sb.WriteString("|-------|-------|------|---------|---------|-------|------|\n")

	type row struct {
		tierKey
		Success, Failure, Total int
	}
	var rows []row
	for k, v := range agg {
		rows = append(rows, row{k, v[0], v[1], v[2]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Agent != rows[j].Agent {
			return rows[i].Agent < rows[j].Agent
		}
		return rows[i].Tier < rows[j].Tier
	})

	for _, r := range rows {
		rate := 0.0
		if r.Total > 0 {
			rate = float64(r.Success) / float64(r.Total) * 100
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %d | %d | %.0f%% |\n",
			r.Agent, r.Model, r.Tier, r.Success, r.Failure, r.Total, rate))
	}
	sb.WriteString("\n")

	totalWork := 0
	tierWork := make(map[int]int)
	for _, r := range rows {
		totalWork += r.Total
		tierWork[r.Tier] += r.Total
	}
	if totalWork > 0 {
		sb.WriteString("### Tier Distribution\n\n")
		for tier := 0; tier < 10; tier++ {
			if w, ok := tierWork[tier]; ok {
				sb.WriteString(fmt.Sprintf("- Tier %d: %d calls (%.0f%%)\n", tier, w, float64(w)/float64(totalWork)*100))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// FilterByConfig returns results matching the given config name.
func FilterByConfig(results []BenchResult, configName string) []BenchResult {
	var filtered []BenchResult
	for _, r := range results {
		if r.Config == configName {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// GenerateAblationReport produces pairwise statistical comparisons between configs.
func GenerateAblationReport(results []BenchResult) string {
	byScenario := groupByScenario(results)
	if len(byScenario) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# Ablation Analysis\n\n")
	produced := false

	for _, scenario := range sortedKeys(byScenario) {
		scenarioResults := byScenario[scenario]
		byConfig := groupByConfig(scenarioResults)
		configs := sortedKeys(byConfig)
		if len(configs) < 2 {
			continue
		}

		sb.WriteString(fmt.Sprintf("## %s\n\n", scenario))

		sb.WriteString("| Baseline | Treatment | Cost Delta | p-value | Sig | Cohen's d | 95% CI |\n")
		sb.WriteString("|----------|-----------|-----------|---------|-----|-----------|--------|\n")

		baselineConfig := configs[0]
		baselineResults := byConfig[baselineConfig]
		bCosts := extractCosts(baselineResults)

		for _, treatConfig := range configs[1:] {
			treatResults := byConfig[treatConfig]
			tCosts := extractCosts(treatResults)

			if len(bCosts) < 2 || len(tCosts) < 2 {
				continue
			}

			_, pValue, _ := WelchTTest(bCosts, tCosts, 0.05)
			d := CohenD(bCosts, tCosts)
			lo, hi := BootstrapCI(bCosts, tCosts, 10000, 0.05)
			sig := significanceMarker(pValue)

			delta := ""
			bMean := mean(bCosts)
			tMean := mean(tCosts)
			if bMean > 0 {
				delta = fmt.Sprintf("%+.0f%%", (tMean-bMean)/bMean*100)
			}

			sb.WriteString(fmt.Sprintf("| %s | %s | %s | %.3f | %s | %.2f | [%.0f%%, %.0f%%] |\n",
				baselineConfig, treatConfig, delta, pValue, sig, d, lo, hi))
			produced = true
		}
		sb.WriteString("\n")
	}

	if !produced {
		return ""
	}
	return sb.String()
}

// GenerateCompetitorReport produces per-config comparisons against a competitor.
func GenerateCompetitorReport(agentfabResults, competitorResults []BenchResult, competitorName string) string {
	if len(agentfabResults) == 0 || len(competitorResults) == 0 {
		return "Insufficient data for competitor comparison.\n"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# AgentFab vs %s\n\n", competitorName))

	afByScenario := groupByScenario(agentfabResults)
	cByScenario := groupByScenario(competitorResults)

	for _, scenario := range sortedKeys(afByScenario) {
		cResults, ok := cByScenario[scenario]
		if !ok || len(cResults) == 0 {
			continue
		}

		afResults := afByScenario[scenario]
		afByConfig := groupByConfig(afResults)

		sb.WriteString(fmt.Sprintf("## %s\n\n", scenario))
		sb.WriteString(fmt.Sprintf("| Config | AgentFab Cost | %s Cost | Delta | p-value | Sig |\n", competitorName))
		sb.WriteString("|--------|--------------|")
		sb.WriteString(strings.Repeat("-", len(competitorName)+7))
		sb.WriteString("|-------|---------|-----|\n")

		cCosts := extractCosts(cResults)
		cMean := mean(cCosts)
		cStd := stddev(cCosts)

		for _, cfg := range sortedKeys(afByConfig) {
			cfgResults := afByConfig[cfg]
			afCosts := extractCosts(cfgResults)
			afMean := mean(afCosts)
			afStd := stddev(afCosts)

			delta := ""
			if cMean > 0 {
				delta = fmt.Sprintf("%+.0f%%", (afMean-cMean)/cMean*100)
			}

			_, pValue, _ := WelchTTest(afCosts, cCosts, 0.05)
			sig := significanceMarker(pValue)

			sb.WriteString(fmt.Sprintf("| %s | $%.2f ± $%.2f | $%.2f ± $%.2f | %s | %.3f | %s |\n",
				cfg, afMean, afStd, cMean, cStd, delta, pValue, sig))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

type aggregated struct {
	MeanCost           float64
	StddevCost         float64
	MeanDuration       float64 // seconds
	StddevDuration     float64
	MeanInputTokens    int64
	MeanOutputTokens   int64
	MeanTasksCompleted int
	MeanTasksFailed    int
	MeanAssertsPassed  int
	MeanAssertsFailed  int
	BuildPassRate      float64
}

func aggregateResults(results []BenchResult) aggregated {
	n := float64(len(results))
	if n == 0 {
		return aggregated{}
	}

	var agg aggregated
	var costs, durations []float64
	var buildPasses int

	for _, r := range results {
		costs = append(costs, r.EstCostUSD)
		durations = append(durations, r.Duration.Seconds())
		agg.MeanInputTokens += r.InputTokens
		agg.MeanOutputTokens += r.OutputTokens
		agg.MeanTasksCompleted += r.TasksCompleted
		agg.MeanTasksFailed += r.TasksFailed
		agg.MeanAssertsPassed += r.AssertsPassed
		agg.MeanAssertsFailed += r.AssertsFailed
		if r.BuildPassed {
			buildPasses++
		}
	}

	agg.MeanCost = mean(costs)
	agg.StddevCost = stddev(costs)
	agg.MeanDuration = mean(durations)
	agg.StddevDuration = stddev(durations)
	agg.MeanInputTokens = int64(float64(agg.MeanInputTokens) / n)
	agg.MeanOutputTokens = int64(float64(agg.MeanOutputTokens) / n)
	agg.MeanTasksCompleted = int(float64(agg.MeanTasksCompleted) / n)
	agg.MeanTasksFailed = int(float64(agg.MeanTasksFailed) / n)
	agg.MeanAssertsPassed = int(float64(agg.MeanAssertsPassed) / n)
	agg.MeanAssertsFailed = int(float64(agg.MeanAssertsFailed) / n)
	agg.BuildPassRate = float64(buildPasses) / n

	return agg
}

func aggregateAgentMetrics(results []BenchResult) map[string]AgentMetric {
	agg := make(map[string]*AgentMetric)
	counts := make(map[string]int)

	for _, r := range results {
		for _, am := range r.AgentMetrics {
			if _, ok := agg[am.Agent]; !ok {
				agg[am.Agent] = &AgentMetric{Agent: am.Agent, Model: am.Model}
			}
			a := agg[am.Agent]
			a.InputTokens += am.InputTokens
			a.OutputTokens += am.OutputTokens
			a.Calls += am.Calls
			a.EstCostUSD += am.EstCostUSD
			counts[am.Agent]++
		}
	}

	result := make(map[string]AgentMetric, len(agg))
	for name, a := range agg {
		n := int64(counts[name])
		if n > 1 {
			a.InputTokens /= n
			a.OutputTokens /= n
			a.Calls /= n
			a.EstCostUSD /= float64(n)
		}
		result[name] = *a
	}
	return result
}

func groupByScenario(results []BenchResult) map[string][]BenchResult {
	m := make(map[string][]BenchResult)
	for _, r := range results {
		m[r.Scenario] = append(m[r.Scenario], r)
	}
	return m
}

func groupByConfig(results []BenchResult) map[string][]BenchResult {
	m := make(map[string][]BenchResult)
	for _, r := range results {
		m[r.Config] = append(m[r.Config], r)
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatDuration(seconds float64) string {
	d := int(seconds)
	if d < 60 {
		return fmt.Sprintf("%ds", d)
	}
	return fmt.Sprintf("%dm%02ds", d/60, d%60)
}

func formatTokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%dK", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

func boolPass(b bool) string {
	if b {
		return "PASS"
	}
	return "FAIL"
}
