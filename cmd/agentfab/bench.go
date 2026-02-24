package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/internal/bench"
	"github.com/razvanmaftei/agentfab/internal/config"
	"github.com/razvanmaftei/agentfab/internal/llm"
	"github.com/spf13/cobra"
)

func benchCmd() *cobra.Command {
	var (
		scenarioFile string
		scenarioDir  string
		all          bool
		runs         int
		outputDir    string
		configFile   string
		debug        bool
		smoke          bool
		competitor     string
		competitorModel string
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run benchmark scenarios and produce comparison tables",
		Long:  "Run standardized tasks under different model configurations and produce cost, quality, and duration comparisons.",
		RunE: func(cmd *cobra.Command, args []string) error {
			td, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load fabric definition: %w\nRun 'agentfab init' first", err)
			}

			var scenarios []bench.Scenario
			if scenarioFile != "" {
				s, err := bench.LoadScenario(scenarioFile)
				if err != nil {
					return fmt.Errorf("load scenario: %w", err)
				}
				scenarios = append(scenarios, *s)
			} else if all || scenarioDir != "" {
				dir := scenarioDir
				if dir == "" {
					dir = "bench/scenarios"
				}
				loaded, err := bench.LoadAllScenarios(dir)
				if err != nil {
					return fmt.Errorf("load scenarios from %s: %w", dir, err)
				}
				scenarios = loaded
			} else {
				return fmt.Errorf("specify --scenario <file>, --dir <dir>, or --all")
			}

			if len(scenarios) == 0 {
				return fmt.Errorf("no scenarios found")
			}

			if smoke {
				runs = 1
			}

			if (cmd.Flags().Changed("runs") || smoke) && runs > 0 {
				for i := range scenarios {
					scenarios[i].Runs = runs
				}
			}

			// Resolve output directory to an absolute path. Post-process tools
			// (e.g., git) run inside temp-dir sandboxes, so relative paths
			// would not resolve correctly.
			if outputDir == "" {
				outputDir = filepath.Join("bench-results", time.Now().Format("2006-01-02_150405"))
			}
			absOut, err := filepath.Abs(outputDir)
			if err != nil {
				return fmt.Errorf("resolve output dir: %w", err)
			}
			outputDir = absOut
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("create output dir: %w", err)
			}

			logLevel := slog.LevelInfo
			if debug {
				logLevel = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: logLevel,
			})))

			factory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
				return llm.NewChatModel(ctx, modelID, nil, td.Providers)
			}

			runner := &bench.Runner{
				BaseFabricDef: td,
				Factory:       factory,
				OutputDir:     outputDir,
				Debug:         debug,
			}

			if competitor != "" {
				if err := bench.CheckCompetitorAvailable(competitor); err != nil {
					return err
				}
			}

			missingKeys := bench.ValidateAPIKeys(td, scenarios)
			if len(missingKeys) > 0 {
				fmt.Fprintf(os.Stderr, "WARNING: Missing API keys detected:\n")
				for _, msg := range missingKeys {
					fmt.Fprintf(os.Stderr, "  - %s\n", msg)
				}
				fmt.Fprintf(os.Stderr, "Configs using those providers will fail. Set the env vars or use --smoke to test.\n\n")
			}

			fmt.Printf("Running %d scenario(s), output: %s\n", len(scenarios), outputDir)
			if smoke {
				fmt.Println("Smoke mode: 1 run per config")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var allResults []bench.BenchResult
			var competitorResults []bench.BenchResult

			for _, s := range scenarios {
				fmt.Printf("\n--- Scenario: %s ---\n", s.Name)
				fmt.Printf("Request: %s\n", truncate(s.Request, 80))
				fmt.Printf("Configs: %d, Runs per config: %d\n", len(s.Configs), s.Runs)

				results, err := runner.RunScenario(ctx, s)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error running %s: %v\n", s.Name, err)
				}
				allResults = append(allResults, results...)

				if competitor != "" {
					fmt.Printf("  Running competitor: %s\n", competitor)
					cr := &bench.CompetitorRunner{
						Tool:      competitor,
						ModelID:   competitorModel,
						OutputDir: outputDir,
					}
					cResults, err := cr.RunScenario(ctx, s)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error running competitor %s on %s: %v\n", competitor, s.Name, err)
					}
					competitorResults = append(competitorResults, cResults...)
				}
			}

			report := bench.GenerateReport(allResults)

			ablationReport := bench.GenerateAblationReport(allResults)
			if ablationReport != "" {
				report += "\n" + ablationReport
			}

			hasAdaptive := false
			for _, r := range allResults {
				if len(r.TierStats) > 0 {
					hasAdaptive = true
					break
				}
			}
			if hasAdaptive {
				report += "\n" + bench.GenerateAdaptiveReport(allResults)
			}

			if len(competitorResults) > 0 {
				report += "\n" + bench.GenerateCompetitorReport(
					allResults, competitorResults, competitor)
			}

			fmt.Println("\n" + report)

			reportPath := filepath.Join(outputDir, "report.md")
			if err := os.WriteFile(reportPath, []byte(report), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write report: %v\n", err)
			} else {
				fmt.Printf("Report written to: %s\n", reportPath)
			}

			jsonPath := filepath.Join(outputDir, "results.json")
			if err := bench.WriteJSON(allResults, jsonPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write JSON: %v\n", err)
			} else {
				fmt.Printf("JSON results written to: %s\n", jsonPath)
			}

			if len(competitorResults) > 0 {
				cPath := filepath.Join(outputDir, "competitor-results.json")
				if err := bench.WriteJSON(competitorResults, cPath); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not write competitor JSON: %v\n", err)
				} else {
					fmt.Printf("Competitor results written to: %s\n", cPath)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&scenarioFile, "scenario", "", "Path to a single scenario YAML file")
	cmd.Flags().StringVar(&scenarioDir, "dir", "", "Directory containing scenario YAML files")
	cmd.Flags().BoolVar(&all, "all", false, "Run all scenarios in bench/scenarios/")
	cmd.Flags().IntVar(&runs, "runs", 0, "Override number of runs per config")
	cmd.Flags().StringVar(&outputDir, "output", "", "Output directory (default: bench-results/<timestamp>)")
	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&smoke, "smoke", false, "Smoke test mode: 1 run per config for quick validation")
	cmd.Flags().StringVar(&competitor, "competitor", "", "Run competitor tool in parallel (e.g., 'aider')")
	cmd.Flags().StringVar(&competitorModel, "competitor-model", "", "Model for competitor tool (e.g., 'claude-sonnet-4-5-20241022')")

	cmd.AddCommand(benchCompareCmd())
	cmd.AddCommand(benchAnalyzeCmd())
	cmd.AddCommand(benchSWEBenchCmd())

	return cmd
}

func benchCompareCmd() *cobra.Command {
	var (
		baselinePath   string
		treatmentPath  string
		baselineName   string
		treatmentName  string
		baselineConfig string
		treatmentConfig string
	)
	cmd := &cobra.Command{
		Use:   "compare",
		Short: "Compare two benchmark result sets with statistical analysis",
		Long:  "Load two JSON result files and produce a statistical comparison report with significance tests.",
		RunE: func(cmd *cobra.Command, args []string) error {
			baseline, err := bench.ReadJSON(baselinePath)
			if err != nil {
				return fmt.Errorf("load baseline: %w", err)
			}
			treatment, err := bench.ReadJSON(treatmentPath)
			if err != nil {
				return fmt.Errorf("load treatment: %w", err)
			}

			if baselineConfig != "" {
				baseline = bench.FilterByConfig(baseline, baselineConfig)
				if len(baseline) == 0 {
					return fmt.Errorf("no baseline results match config %q", baselineConfig)
				}
			}
			if treatmentConfig != "" {
				treatment = bench.FilterByConfig(treatment, treatmentConfig)
				if len(treatment) == 0 {
					return fmt.Errorf("no treatment results match config %q", treatmentConfig)
				}
			}

			if baselineName == "" {
				baselineName = filepath.Base(filepath.Dir(baselinePath))
				if baselineConfig != "" {
					baselineName += "/" + baselineConfig
				}
			}
			if treatmentName == "" {
				treatmentName = filepath.Base(filepath.Dir(treatmentPath))
				if treatmentConfig != "" {
					treatmentName += "/" + treatmentConfig
				}
			}

			report := bench.GenerateComparisonReport(baseline, treatment, baselineName, treatmentName)
			fmt.Println(report)

			return nil
		},
	}

	cmd.Flags().StringVar(&baselinePath, "baseline", "", "Path to baseline results.json (required)")
	cmd.Flags().StringVar(&treatmentPath, "treatment", "", "Path to treatment results.json (required)")
	cmd.Flags().StringVar(&baselineName, "baseline-name", "", "Display name for baseline (default: directory name)")
	cmd.Flags().StringVar(&treatmentName, "treatment-name", "", "Display name for treatment (default: directory name)")
	cmd.Flags().StringVar(&baselineConfig, "baseline-config", "", "Filter baseline results by config name")
	cmd.Flags().StringVar(&treatmentConfig, "treatment-config", "", "Filter treatment results by config name")
	cmd.MarkFlagRequired("baseline")
	cmd.MarkFlagRequired("treatment")

	return cmd
}

func benchAnalyzeCmd() *cobra.Command {
	var (
		resultsDir  string
		scenarioDir string
	)
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Post-hoc analysis of benchmark artifacts",
		Long:  "Scan completed benchmark run artifacts for syntax errors, missing files, git health, and replay assertions.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if resultsDir == "" {
				entries, err := os.ReadDir("bench-results")
				if err != nil {
					return fmt.Errorf("no bench-results directory found")
				}
				latest := ""
				for _, e := range entries {
					if e.IsDir() && e.Name() > latest {
						latest = e.Name()
					}
				}
				if latest == "" {
					return fmt.Errorf("no result sets found in bench-results/")
				}
				resultsDir = filepath.Join("bench-results", latest)
			}

			sDir := scenarioDir
			if sDir == "" {
				sDir = "bench/scenarios"
			}
			scenarios, err := bench.LoadAllScenarios(sDir)
			if err != nil {
				return fmt.Errorf("load scenarios: %w", err)
			}

			fmt.Printf("Analyzing: %s\n", resultsDir)
			fmt.Printf("Scenarios: %d loaded for assertion replay\n\n", len(scenarios))

			results, err := bench.AnalyzeResults(resultsDir, scenarios)
			if err != nil {
				return fmt.Errorf("analyze: %w", err)
			}

			report := bench.GenerateAnalysisReport(results)
			fmt.Println(report)

			reportPath := filepath.Join(resultsDir, "analysis.md")
			if err := os.WriteFile(reportPath, []byte(report), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write analysis report: %v\n", err)
			} else {
				fmt.Printf("Analysis written to: %s\n", reportPath)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&resultsDir, "results", "", "Path to results directory (default: latest in bench-results/)")
	cmd.Flags().StringVar(&scenarioDir, "scenarios", "", "Path to scenarios directory (default: bench/scenarios)")

	return cmd
}

func benchSWEBenchCmd() *cobra.Command {
	var (
		datasetPath string
		instances   string
		repos       string
		limit       int
		outputDir   string
		configFile  string
		cacheDir    string
		modelName   string
		debug       bool
		hints       bool
	)
	cmd := &cobra.Command{
		Use:   "swebench",
		Short: "Run SWE-bench instances and produce predictions",
		Long: `Run agentfab on SWE-bench task instances and produce predictions.jsonl
for evaluation with the standard SWE-bench harness.

First, export the dataset:
  cd agentfab-bench/swebench && python export_dataset.py

Then run:
  agentfab bench swebench --dataset swebench/swebench_verified.jsonl --limit 5

Evaluate with:
  python -m swebench.harness.run_evaluation \
    --dataset_name SWE-bench/SWE-bench_Verified \
    --predictions_path predictions.jsonl \
    --max_workers 8 --run_id agentfab`,
		RunE: func(cmd *cobra.Command, args []string) error {
			td, err := config.LoadFabricDef(configFile)
			if err != nil {
				return fmt.Errorf("load fabric definition: %w\nRun 'agentfab init' first", err)
			}

			allInstances, err := bench.LoadSWEBenchInstances(datasetPath)
			if err != nil {
				return fmt.Errorf("load dataset: %w", err)
			}

			var idList, repoList []string
			if instances != "" {
				for _, id := range splitComma(instances) {
					idList = append(idList, id)
				}
			}
			if repos != "" {
				for _, r := range splitComma(repos) {
					repoList = append(repoList, r)
				}
			}
			filtered := bench.FilterSWEBenchInstances(allInstances, idList, repoList)

			if limit > 0 && len(filtered) > limit {
				filtered = filtered[:limit]
			}

			if len(filtered) == 0 {
				return fmt.Errorf("no instances match filters (total: %d)", len(allInstances))
			}

			if outputDir == "" {
				outputDir = filepath.Join("bench-results", "swebench-"+time.Now().Format("2006-01-02_150405"))
			}
			absOut, err := filepath.Abs(outputDir)
			if err != nil {
				return fmt.Errorf("resolve output dir: %w", err)
			}
			outputDir = absOut
			if err := os.MkdirAll(outputDir, 0755); err != nil {
				return fmt.Errorf("create output dir: %w", err)
			}

			if cacheDir == "" {
				cacheDir = filepath.Join(outputDir, ".repo-cache")
			}
			absCacheDir, err := filepath.Abs(cacheDir)
			if err != nil {
				return fmt.Errorf("resolve cache dir: %w", err)
			}
			cacheDir = absCacheDir
			if err := os.MkdirAll(cacheDir, 0755); err != nil {
				return fmt.Errorf("create cache dir: %w", err)
			}

			if modelName == "" {
				modelName = "agentfab-v1"
			}

			logLevel := slog.LevelInfo
			if debug {
				logLevel = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: logLevel,
			})))

			factory := func(ctx context.Context, modelID string) (model.ChatModel, error) {
				return llm.NewChatModel(ctx, modelID, nil, td.Providers)
			}

			runner := &bench.SWEBenchRunner{
				BaseFabricDef: td,
				Factory:       factory,
				OutputDir:     outputDir,
				CacheDir:      cacheDir,
				ModelName:     modelName,
				Debug:         debug,
				Hints:         hints,
			}

			fmt.Printf("SWE-bench: %d instances, output: %s\n", len(filtered), outputDir)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var results []bench.SWEBenchResult
			var predictions []bench.SWEBenchPrediction

			for i, inst := range filtered {
				select {
				case <-ctx.Done():
					break
				default:
				}

				fmt.Printf("\n[%d/%d] %s (%s)\n", i+1, len(filtered), inst.InstanceID, inst.Repo)

				result, pred, err := runner.RunInstance(ctx, inst)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  Error: %v\n", err)
				}

				results = append(results, result)
				if !result.PatchEmpty {
					predictions = append(predictions, pred)
				}

				// Write incremental predictions (crash-safe).
				predPath := filepath.Join(outputDir, "predictions.jsonl")
				if wErr := bench.WritePredictions(predictions, predPath); wErr != nil {
					fmt.Fprintf(os.Stderr, "  Warning: could not write predictions: %v\n", wErr)
				}

				fmt.Printf("  Duration: %s, Cost: $%.2f, Patch: %v\n",
					result.Duration.Round(time.Second), result.EstCostUSD, !result.PatchEmpty)
			}

			report := bench.GenerateSWEBenchReport(results)
			fmt.Println("\n" + report)

			reportPath := filepath.Join(outputDir, "report.md")
			if err := os.WriteFile(reportPath, []byte(report), 0644); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write report: %v\n", err)
			} else {
				fmt.Printf("Report written to: %s\n", reportPath)
			}

			resultsPath := filepath.Join(outputDir, "results.json")
			if err := bench.WriteSWEBenchResultsJSON(results, resultsPath); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: could not write results JSON: %v\n", err)
			} else {
				fmt.Printf("Results written to: %s\n", resultsPath)
			}

			predPath := filepath.Join(outputDir, "predictions.jsonl")
			fmt.Printf("Predictions written to: %s (%d patches)\n", predPath, len(predictions))
			fmt.Printf("\nTo evaluate:\n")
			fmt.Printf("  python -m swebench.harness.run_evaluation \\\n")
			fmt.Printf("    --dataset_name SWE-bench/SWE-bench_Verified \\\n")
			fmt.Printf("    --predictions_path %s \\\n", predPath)
			fmt.Printf("    --max_workers 8 \\\n")
			fmt.Printf("    --run_id agentfab\n")

			return nil
		},
	}

	cmd.Flags().StringVar(&datasetPath, "dataset", "", "Path to SWE-bench JSONL dataset (required)")
	cmd.Flags().StringVar(&instances, "instances", "", "Comma-separated instance IDs to run")
	cmd.Flags().StringVar(&repos, "repos", "", "Comma-separated repo names to filter (e.g., 'django/django')")
	cmd.Flags().IntVar(&limit, "limit", 0, "Max instances to run (0 = all)")
	cmd.Flags().StringVar(&outputDir, "output", "", "Output directory")
	cmd.Flags().StringVar(&configFile, "config", "agents.yaml", "Path to agents.yaml")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Directory for cached repo clones (default: inside output)")
	cmd.Flags().StringVar(&modelName, "model-name", "agentfab-v1", "Model name for predictions JSONL")
	cmd.Flags().BoolVar(&debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&hints, "hints", true, "Include issue hints_text in the request")
	cmd.MarkFlagRequired("dataset")

	return cmd
}

func splitComma(s string) []string {
	var result []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
