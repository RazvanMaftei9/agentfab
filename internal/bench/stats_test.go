package bench

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWelchTTest_SignificantDifference(t *testing.T) {
	// Two clearly different distributions.
	a := []float64{10, 12, 11, 13, 10, 12}
	b := []float64{20, 22, 21, 23, 20, 22}

	tStat, pValue, sig := WelchTTest(a, b, 0.05)

	if !sig {
		t.Errorf("expected significant difference, got p=%.4f", pValue)
	}
	if pValue >= 0.05 {
		t.Errorf("expected p < 0.05, got %.4f", pValue)
	}
	if tStat >= 0 {
		t.Errorf("expected negative t-stat (a < b), got %.4f", tStat)
	}
}

func TestWelchTTest_NoSignificantDifference(t *testing.T) {
	a := []float64{10, 11, 10, 11, 10}
	b := []float64{10, 11, 10, 11, 10}

	_, pValue, sig := WelchTTest(a, b, 0.05)

	if sig {
		t.Errorf("expected no significant difference, got p=%.4f", pValue)
	}
}

func TestWelchTTest_InsufficientData(t *testing.T) {
	a := []float64{10}
	b := []float64{20}

	_, pValue, sig := WelchTTest(a, b, 0.05)

	if sig {
		t.Error("expected not significant with n=1")
	}
	if pValue != 1 {
		t.Errorf("expected p=1 with insufficient data, got %.4f", pValue)
	}
}

func TestCohenD_LargeEffect(t *testing.T) {
	a := []float64{10, 12, 11, 13, 10}
	b := []float64{20, 22, 21, 23, 20}

	d := CohenD(a, b)

	// Cohen's d should be large (> 0.8 or < -0.8).
	if math.Abs(d) < 0.8 {
		t.Errorf("expected large effect size, got d=%.2f", d)
	}
}

func TestCohenD_NoEffect(t *testing.T) {
	a := []float64{10, 11, 10, 11, 10}
	b := []float64{10, 11, 10, 11, 10}

	d := CohenD(a, b)

	if math.Abs(d) > 0.01 {
		t.Errorf("expected ~0 effect size, got d=%.2f", d)
	}
}

func TestCohenD_InsufficientData(t *testing.T) {
	d := CohenD([]float64{1}, []float64{2})
	if d != 0 {
		t.Errorf("expected 0 with insufficient data, got %.2f", d)
	}
}

func TestBootstrapCI_ClearDifference(t *testing.T) {
	baseline := []float64{100, 110, 105, 95, 100}
	treatment := []float64{50, 55, 52, 48, 50}

	lower, upper := BootstrapCI(baseline, treatment, 10000, 0.05)

	// Treatment is ~50% cheaper, CI should be entirely negative.
	if lower >= 0 {
		t.Errorf("expected negative lower bound, got %.1f%%", lower)
	}
	if upper >= 0 {
		t.Errorf("expected negative upper bound, got %.1f%%", upper)
	}
	// Should be around -50%.
	midpoint := (lower + upper) / 2
	if midpoint > -40 || midpoint < -60 {
		t.Errorf("expected CI around -50%%, got [%.1f%%, %.1f%%]", lower, upper)
	}
}

func TestBootstrapCI_NoDifference(t *testing.T) {
	a := []float64{100, 100, 100, 100, 100}
	b := []float64{100, 100, 100, 100, 100}

	lower, upper := BootstrapCI(a, b, 10000, 0.05)

	if math.Abs(lower) > 1 || math.Abs(upper) > 1 {
		t.Errorf("expected CI around 0, got [%.1f%%, %.1f%%]", lower, upper)
	}
}

func TestBootstrapCI_EmptyInput(t *testing.T) {
	lower, upper := BootstrapCI(nil, []float64{1, 2}, 1000, 0.05)
	if lower != 0 || upper != 0 {
		t.Errorf("expected [0, 0] for empty input, got [%.1f, %.1f]", lower, upper)
	}
}

func TestComparisonSummary_ProducesMarkdown(t *testing.T) {
	baseline := []BenchResult{
		{EstCostUSD: 2.50, Duration: 4 * time.Minute, AssertsPassed: 5, AssertsFailed: 0},
		{EstCostUSD: 2.30, Duration: 3*time.Minute + 50*time.Second, AssertsPassed: 5, AssertsFailed: 0},
		{EstCostUSD: 2.60, Duration: 4*time.Minute + 10*time.Second, AssertsPassed: 5, AssertsFailed: 0},
	}
	treatment := []BenchResult{
		{EstCostUSD: 1.20, Duration: 3*time.Minute + 30*time.Second, AssertsPassed: 5, AssertsFailed: 0},
		{EstCostUSD: 1.10, Duration: 3*time.Minute + 20*time.Second, AssertsPassed: 5, AssertsFailed: 0},
		{EstCostUSD: 1.30, Duration: 3*time.Minute + 40*time.Second, AssertsPassed: 5, AssertsFailed: 0},
	}

	result := ComparisonSummary(baseline, treatment, "all-opus", "heterogeneous")

	if !strings.Contains(result, "Cost (USD)") {
		t.Error("expected Cost row in comparison table")
	}
	if !strings.Contains(result, "Duration") {
		t.Error("expected Duration row in comparison table")
	}
	if !strings.Contains(result, "all-opus") {
		t.Error("expected baseline name in header")
	}
	if !strings.Contains(result, "heterogeneous") {
		t.Error("expected treatment name in header")
	}
	// Should show significance for cost (clearly different).
	if !strings.Contains(result, "*") {
		t.Error("expected significance markers for cost comparison")
	}
}

func TestComparisonSummary_EmptyInput(t *testing.T) {
	result := ComparisonSummary(nil, nil, "a", "b")
	if !strings.Contains(result, "Insufficient") {
		t.Error("expected insufficient data message")
	}
}

func TestSignificanceMarker(t *testing.T) {
	tests := []struct {
		p    float64
		want string
	}{
		{0.0001, "***"},
		{0.005, "**"},
		{0.03, "*"},
		{0.1, ""},
		{0.5, ""},
	}
	for _, tt := range tests {
		got := significanceMarker(tt.p)
		if got != tt.want {
			t.Errorf("significanceMarker(%.4f) = %q, want %q", tt.p, got, tt.want)
		}
	}
}

func TestVariance(t *testing.T) {
	vals := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	v := variance(vals)
	// Known variance for this dataset: ~4.571
	if math.Abs(v-4.571) > 0.01 {
		t.Errorf("expected variance ~4.571, got %.3f", v)
	}
}

func TestRegIncBeta_BoundaryValues(t *testing.T) {
	// I_0(a, b) = 0
	if v := regIncBeta(0, 1, 1); v != 0 {
		t.Errorf("expected I_0(1,1) = 0, got %f", v)
	}
	// I_1(a, b) = 1
	if v := regIncBeta(1, 1, 1); v != 1 {
		t.Errorf("expected I_1(1,1) = 1, got %f", v)
	}
	// I_0.5(1, 1) = 0.5 (uniform beta)
	v := regIncBeta(0.5, 1, 1)
	if math.Abs(v-0.5) > 0.001 {
		t.Errorf("expected I_0.5(1,1) = 0.5, got %f", v)
	}
}

func TestFilterByConfig(t *testing.T) {
	results := []BenchResult{
		{Scenario: "s1", Config: "all-opus", EstCostUSD: 2.0},
		{Scenario: "s1", Config: "all-sonnet", EstCostUSD: 1.0},
		{Scenario: "s1", Config: "all-opus", EstCostUSD: 2.1},
	}
	filtered := FilterByConfig(results, "all-opus")
	if len(filtered) != 2 {
		t.Errorf("expected 2 all-opus results, got %d", len(filtered))
	}
	filtered = FilterByConfig(results, "nonexistent")
	if len(filtered) != 0 {
		t.Errorf("expected 0 results for nonexistent config, got %d", len(filtered))
	}
}

func TestGenerateAblationReport_MultipleConfigs(t *testing.T) {
	results := []BenchResult{
		{Scenario: "test", Config: "all-opus", EstCostUSD: 2.50, Duration: 4 * time.Minute, AssertsPassed: 5},
		{Scenario: "test", Config: "all-opus", EstCostUSD: 2.30, Duration: 3*time.Minute + 50*time.Second, AssertsPassed: 5},
		{Scenario: "test", Config: "all-opus", EstCostUSD: 2.60, Duration: 4*time.Minute + 10*time.Second, AssertsPassed: 5},
		{Scenario: "test", Config: "heterogeneous", EstCostUSD: 1.20, Duration: 3*time.Minute + 30*time.Second, AssertsPassed: 5},
		{Scenario: "test", Config: "heterogeneous", EstCostUSD: 1.10, Duration: 3*time.Minute + 20*time.Second, AssertsPassed: 5},
		{Scenario: "test", Config: "heterogeneous", EstCostUSD: 1.30, Duration: 3*time.Minute + 40*time.Second, AssertsPassed: 5},
	}

	report := GenerateAblationReport(results)

	if !strings.Contains(report, "Ablation Analysis") {
		t.Error("expected Ablation Analysis header")
	}
	if !strings.Contains(report, "all-opus") {
		t.Error("expected all-opus in report")
	}
	if !strings.Contains(report, "heterogeneous") {
		t.Error("expected heterogeneous in report")
	}
	if !strings.Contains(report, "p-value") {
		t.Error("expected p-value column")
	}
	// Should show significance since costs are clearly different.
	if !strings.Contains(report, "*") {
		t.Error("expected significance markers")
	}
}

func TestGenerateAblationReport_SingleConfig(t *testing.T) {
	results := []BenchResult{
		{Scenario: "test", Config: "only-one", EstCostUSD: 1.0},
	}
	report := GenerateAblationReport(results)
	if report != "" {
		t.Error("expected empty report for single config")
	}
}

func TestGenerateCompetitorReport(t *testing.T) {
	afResults := []BenchResult{
		{Scenario: "test", Config: "all-opus", EstCostUSD: 2.50, AssertsPassed: 5},
		{Scenario: "test", Config: "all-opus", EstCostUSD: 2.30, AssertsPassed: 5},
		{Scenario: "test", Config: "heterogeneous", EstCostUSD: 1.20, AssertsPassed: 5},
		{Scenario: "test", Config: "heterogeneous", EstCostUSD: 1.10, AssertsPassed: 5},
	}
	cResults := []BenchResult{
		{Scenario: "test", Config: "aider", EstCostUSD: 1.80, AssertsPassed: 4},
		{Scenario: "test", Config: "aider", EstCostUSD: 1.90, AssertsPassed: 3},
	}

	report := GenerateCompetitorReport(afResults, cResults, "aider")

	if !strings.Contains(report, "AgentFab vs aider") {
		t.Error("expected competitor header")
	}
	if !strings.Contains(report, "all-opus") {
		t.Error("expected all-opus config row")
	}
	if !strings.Contains(report, "heterogeneous") {
		t.Error("expected heterogeneous config row")
	}
}

func TestParseTokenCount(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"856", 856},
		{"1.2k", 1200},
		{"1.2K", 1200},
		{"1,200", 1200},
		{"2.5M", 2500000},
		{"0", 0},
		{"invalid", 0},
	}
	for _, tt := range tests {
		got := parseTokenCount(tt.input)
		if got != tt.want {
			t.Errorf("parseTokenCount(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestMatchPathSuffix(t *testing.T) {
	tests := []struct {
		rel, suffix string
		want        bool
	}{
		// Exact match.
		{"packages/shared-utils/index.js", "packages/shared-utils/index.js", true},
		// Agent subdirectory prefix.
		{"developer/packages/shared-utils/index.js", "packages/shared-utils/index.js", true},
		// Deeper nesting.
		{"developer/sub/packages/shared-utils/index.js", "packages/shared-utils/index.js", true},
		// Simple filename (no dir in suffix).
		{"developer/index.html", "index.html", true},
		// No match.
		{"developer/other/file.js", "packages/shared-utils/index.js", false},
		// Partial directory name shouldn't match.
		{"developer/xpackages/shared-utils/index.js", "packages/shared-utils/index.js", false},
	}
	for _, tt := range tests {
		got := matchPathSuffix(tt.rel, tt.suffix)
		if got != tt.want {
			t.Errorf("matchPathSuffix(%q, %q) = %v, want %v", tt.rel, tt.suffix, got, tt.want)
		}
	}
}

func TestRecursiveGlobWithSubdirs(t *testing.T) {
	// Create a temp directory tree mimicking the artifact layout.
	root := t.TempDir()
	dirs := []string{
		"developer/packages/shared-utils",
		"developer/packages/web-app",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(root+"/"+d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := []string{
		"developer/packages/shared-utils/index.js",
		"developer/packages/web-app/package.json",
		"developer/index.html",
	}
	for _, f := range files {
		if err := os.WriteFile(root+"/"+f, []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Path with directories should match through agent subdirectory.
	if !recursiveGlob(root, "packages/shared-utils/index.js") {
		t.Error("recursiveGlob should find packages/shared-utils/index.js")
	}
	// Simple filename glob should still work.
	if !recursiveGlob(root, "*.html") {
		t.Error("recursiveGlob should find *.html")
	}
	// Non-existent path should not match.
	if recursiveGlob(root, "packages/missing/file.js") {
		t.Error("recursiveGlob should not match missing path")
	}

	// recursiveGlobFiles with directory path.
	matches := recursiveGlobFiles(root, "packages/web-app/package.json")
	if len(matches) != 1 {
		t.Errorf("recursiveGlobFiles expected 1 match, got %d", len(matches))
	}
}
