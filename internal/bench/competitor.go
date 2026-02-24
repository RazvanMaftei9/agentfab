package bench

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type CompetitorRunner struct {
	Tool      string // "aider"
	ModelID   string // e.g. "claude-sonnet-4-5-20241022"
	OutputDir string
}

func (cr *CompetitorRunner) RunScenario(ctx context.Context, s Scenario) ([]BenchResult, error) {
	runs := s.Runs
	if runs <= 0 {
		runs = 3
	}

	var results []BenchResult
	for i := 0; i < runs; i++ {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		slog.Info("competitor: starting run",
			"tool", cr.Tool, "scenario", s.Name, "run", i+1)

		result, err := cr.runSingle(ctx, s, i)
		if err != nil {
			result.Error = err.Error()
			slog.Warn("competitor: run failed",
				"tool", cr.Tool, "scenario", s.Name, "run", i+1, "error", err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (cr *CompetitorRunner) runSingle(ctx context.Context, s Scenario, idx int) (BenchResult, error) {
	configName := cr.Tool
	if cr.ModelID != "" {
		configName = cr.Tool + "/" + cr.ModelID
	}
	result := BenchResult{
		Scenario: s.Name,
		Config:   configName,
		RunIndex: idx,
	}

	workDir := filepath.Join(cr.OutputDir, s.Name, cr.Tool, fmt.Sprintf("run-%d", idx))
	artifactsDir := filepath.Join(workDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0755); err != nil {
		return result, fmt.Errorf("create work dir: %w", err)
	}

	start := time.Now()

	switch cr.Tool {
	case "aider":
		if err := cr.runAider(ctx, s, artifactsDir, &result); err != nil {
			result.Duration = time.Since(start)
			return result, err
		}
	default:
		return result, fmt.Errorf("unsupported competitor tool: %s", cr.Tool)
	}

	result.Duration = time.Since(start)

	passed, failed := RunAssertions(s.Assertions, artifactsDir, workDir)
	result.AssertsPassed = passed
	result.AssertsFailed = failed
	result.BuildPassed = allBuildsPassed(s.Assertions, artifactsDir, workDir)

	return result, nil
}

func (cr *CompetitorRunner) runAider(ctx context.Context, s Scenario, workDir string, result *BenchResult) error {
	if _, err := exec.LookPath("aider"); err != nil {
		return fmt.Errorf("aider not found in PATH: %w", err)
	}

	args := []string{
		"--no-git",
		"--yes",
		"--no-auto-commits",
	}
	if cr.ModelID != "" {
		args = append(args, "--model", cr.ModelID)
	}
	args = append(args, "--message", s.Request)

	cmd := exec.CommandContext(ctx, "aider", args...)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	parseAiderOutput(stdout.String()+stderr.String(), result)

	if err != nil {
		return fmt.Errorf("aider execution: %w\nstderr: %s", err, stderr.String())
	}

	return nil
}

// Aider prints lines like: Tokens: 1.2k sent, 856 received. Cost: $0.01
var (
	aiderTokenRe = regexp.MustCompile(`Tokens:\s*([\d,.]+[kKmM]?)\s*sent,\s*([\d,.]+[kKmM]?)\s*received`)
	aiderCostRe  = regexp.MustCompile(`Cost:\s*\$([\d.]+)`)
)

func parseAiderOutput(output string, result *BenchResult) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		if m := aiderTokenRe.FindStringSubmatch(line); len(m) >= 3 {
			result.InputTokens += parseTokenCount(m[1])
			result.OutputTokens += parseTokenCount(m[2])
			result.TotalCalls++
		}
		if m := aiderCostRe.FindStringSubmatch(line); len(m) >= 2 {
			if cost, err := strconv.ParseFloat(m[1], 64); err == nil {
				result.EstCostUSD += cost
			}
		}
	}
}

func parseTokenCount(s string) int64 {
	s = strings.ReplaceAll(s, ",", "")
	multiplier := 1.0
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "k") || strings.HasSuffix(s, "K") {
		multiplier = 1000
		s = s[:len(s)-1]
	} else if strings.HasSuffix(s, "m") || strings.HasSuffix(s, "M") {
		multiplier = 1000000
		s = s[:len(s)-1]
	}
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(val * multiplier)
}

func CheckCompetitorAvailable(tool string) error {
	switch tool {
	case "aider":
		if _, err := exec.LookPath("aider"); err != nil {
			return fmt.Errorf("aider not found in PATH; install with: pip install aider-chat")
		}
		return nil
	default:
		return fmt.Errorf("unsupported competitor tool: %s", tool)
	}
}
