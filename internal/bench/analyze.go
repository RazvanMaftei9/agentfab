package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type AnalysisResult struct {
	Scenario string
	Config   string
	RunIndex int

	TotalFiles    int
	CodeFiles     int
	ConfigFiles   int
	DocFiles      int
	GitCommits    int
	GitRepos      int

	AssertsPassed int
	AssertsFailed int
	SyntaxErrors  []SyntaxError
	Issues        []string
}

type SyntaxError struct {
	File    string
	Lang    string
	Message string
}

// AnalyzeResults runs post-hoc analysis on completed benchmark runs.
func AnalyzeResults(resultsDir string, scenarios []Scenario) ([]AnalysisResult, error) {
	var results []AnalysisResult

	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return nil, fmt.Errorf("read results dir: %w", err)
	}

	scenarioMap := make(map[string]Scenario)
	for _, s := range scenarios {
		scenarioMap[s.Name] = s
	}

	for _, scenarioEntry := range entries {
		if !scenarioEntry.IsDir() {
			continue
		}
		scenarioName := scenarioEntry.Name()
		scenarioDir := filepath.Join(resultsDir, scenarioName)

		configEntries, err := os.ReadDir(scenarioDir)
		if err != nil {
			continue
		}

		for _, configEntry := range configEntries {
			if !configEntry.IsDir() {
				continue
			}
			configName := configEntry.Name()
			configDir := filepath.Join(scenarioDir, configName)

			runEntries, err := os.ReadDir(configDir)
			if err != nil {
				continue
			}

			for _, runEntry := range runEntries {
				if !runEntry.IsDir() || !strings.HasPrefix(runEntry.Name(), "run-") {
					continue
				}
				runDir := filepath.Join(configDir, runEntry.Name())
				idx := 0
				fmt.Sscanf(runEntry.Name(), "run-%d", &idx)

				ar := analyzeRun(runDir, scenarioName, configName, idx, scenarioMap)
				results = append(results, ar)
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Scenario != results[j].Scenario {
			return results[i].Scenario < results[j].Scenario
		}
		if results[i].Config != results[j].Config {
			return results[i].Config < results[j].Config
		}
		return results[i].RunIndex < results[j].RunIndex
	})

	return results, nil
}

func analyzeRun(runDir, scenario, config string, idx int, scenarios map[string]Scenario) AnalysisResult {
	ar := AnalysisResult{
		Scenario: scenario,
		Config:   config,
		RunIndex: idx,
	}

	artifactsDir := filepath.Join(runDir, "shared", "artifacts")

	filepath.Walk(artifactsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.Contains(path, "/.git/") {
			return nil
		}
		ar.TotalFiles++
		ext := strings.ToLower(filepath.Ext(info.Name()))
		switch ext {
		case ".py", ".js", ".jsx", ".ts", ".tsx", ".go", ".html", ".css", ".java", ".rs", ".rb", ".php":
			ar.CodeFiles++
		case ".md":
			ar.DocFiles++
		}
		name := strings.ToLower(info.Name())
		switch name {
		case "package.json", "go.mod", "cargo.toml", "requirements.txt", "pyproject.toml",
			"makefile", "dockerfile", ".gitignore", "tsconfig.json":
			ar.ConfigFiles++
		}
		return nil
	})

	gitDirs := findGitDirs(artifactsDir)
	ar.GitRepos = len(gitDirs)
	for _, gitDir := range gitDirs {
		repo := filepath.Dir(gitDir)
		out, err := exec.Command("git", "-C", repo, "rev-list", "--count", "HEAD").Output()
		if err == nil {
			count := 0
			fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &count)
			ar.GitCommits += count
		}
	}

	if s, ok := scenarios[scenario]; ok {
		ar.AssertsPassed, ar.AssertsFailed = RunAssertions(s.Assertions, artifactsDir, runDir)
	}

	ar.SyntaxErrors = syntaxCheck(artifactsDir)

	if ar.TotalFiles == 0 {
		ar.Issues = append(ar.Issues, "NO_ARTIFACTS: no files produced")
	}
	if ar.CodeFiles == 0 && ar.TotalFiles > 0 {
		ar.Issues = append(ar.Issues, "NO_CODE: files exist but no code files")
	}
	if ar.GitRepos == 0 && ar.CodeFiles > 0 {
		ar.Issues = append(ar.Issues, "NO_GIT: code files exist but no git repo")
	}
	if ar.GitCommits == 0 && ar.GitRepos > 0 {
		ar.Issues = append(ar.Issues, "NO_COMMITS: git repo exists but no commits")
	}
	if len(ar.SyntaxErrors) > 0 {
		ar.Issues = append(ar.Issues, fmt.Sprintf("SYNTAX_ERRORS: %d files failed syntax check", len(ar.SyntaxErrors)))
	}
	if ar.AssertsFailed > 0 {
		ar.Issues = append(ar.Issues, fmt.Sprintf("ASSERT_FAIL: %d/%d assertions failed",
			ar.AssertsFailed, ar.AssertsPassed+ar.AssertsFailed))
	}

	return ar
}

func findGitDirs(root string) []string {
	var dirs []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() && info.Name() == ".git" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	return dirs
}

func syntaxCheck(artifactsDir string) []SyntaxError {
	var errors []SyntaxError

	filepath.Walk(artifactsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(path, "/.git/") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(info.Name()))
		switch ext {
		case ".py":
			out, err := exec.Command("python3", "-m", "py_compile", path).CombinedOutput()
			if err != nil {
				errors = append(errors, SyntaxError{
					File:    path,
					Lang:    "python",
					Message: truncateStr(string(out), 200),
				})
			}
		case ".js", ".jsx":
			out, err := exec.Command("node", "--check", path).CombinedOutput()
			if err != nil {
				// node --check doesn't work on JSX, skip those failures.
				if ext == ".jsx" {
					return nil
				}
				errors = append(errors, SyntaxError{
					File:    path,
					Lang:    "javascript",
					Message: truncateStr(string(out), 200),
				})
			}
		}
		return nil
	})

	return errors
}

func truncateStr(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// GenerateAnalysisReport produces a Markdown report from analysis results.
func GenerateAnalysisReport(results []AnalysisResult) string {
	var b strings.Builder

	b.WriteString("# Post-Hoc Artifact Analysis\n\n")

	b.WriteString("## Summary\n\n")
	b.WriteString("| Scenario | Config | Files | Code | Cfg | Git | Commits | Asserts | Syntax | Issues |\n")
	b.WriteString("|----------|--------|-------|------|-----|-----|---------|---------|--------|--------|\n")

	totalIssues := 0
	totalSyntaxErrors := 0
	for _, ar := range results {
		assertStr := fmt.Sprintf("%d/%d", ar.AssertsPassed, ar.AssertsPassed+ar.AssertsFailed)
		issueStr := ""
		if len(ar.Issues) > 0 {
			issueStr = strings.Join(ar.Issues, ", ")
			totalIssues++
		}
		syntaxStr := "OK"
		if len(ar.SyntaxErrors) > 0 {
			syntaxStr = fmt.Sprintf("%d err", len(ar.SyntaxErrors))
			totalSyntaxErrors += len(ar.SyntaxErrors)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %d | %d | %d | %s | %s | %s |\n",
			ar.Scenario, ar.Config, ar.TotalFiles, ar.CodeFiles, ar.ConfigFiles,
			ar.GitRepos, ar.GitCommits, assertStr, syntaxStr, issueStr))
	}

	b.WriteString(fmt.Sprintf("\n**Total runs**: %d | **Runs with issues**: %d | **Total syntax errors**: %d\n\n",
		len(results), totalIssues, totalSyntaxErrors))

	b.WriteString("## Assertion Pass Rates by Config\n\n")
	configStats := make(map[string][2]int) // config -> [passed, total]
	for _, ar := range results {
		stats := configStats[ar.Config]
		stats[0] += ar.AssertsPassed
		stats[1] += ar.AssertsPassed + ar.AssertsFailed
		configStats[ar.Config] = stats
	}
	b.WriteString("| Config | Passed | Total | Rate |\n")
	b.WriteString("|--------|--------|-------|------|\n")
	for _, config := range sortedKeys(configStats) {
		stats := configStats[config]
		rate := 0.0
		if stats[1] > 0 {
			rate = float64(stats[0]) / float64(stats[1]) * 100
		}
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %.0f%% |\n", config, stats[0], stats[1], rate))
	}

	if totalSyntaxErrors > 0 {
		b.WriteString("\n## Syntax Errors\n\n")
		for _, ar := range results {
			for _, se := range ar.SyntaxErrors {
				shortPath := se.File
				if idx := strings.Index(shortPath, "/shared/artifacts/"); idx >= 0 {
					shortPath = shortPath[idx+len("/shared/artifacts/"):]
				}
				b.WriteString(fmt.Sprintf("- **%s/%s**: `%s` (%s) — %s\n",
					ar.Scenario, ar.Config, shortPath, se.Lang, se.Message))
			}
		}
	}

	var emptyRuns []string
	for _, ar := range results {
		if ar.TotalFiles == 0 {
			emptyRuns = append(emptyRuns, fmt.Sprintf("%s/%s", ar.Scenario, ar.Config))
		}
	}
	if len(emptyRuns) > 0 {
		b.WriteString("\n## Empty Runs (no artifacts)\n\n")
		for _, r := range emptyRuns {
			b.WriteString(fmt.Sprintf("- %s\n", r))
		}
	}

	return b.String()
}

