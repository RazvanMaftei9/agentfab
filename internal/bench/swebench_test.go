package bench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSWEBenchInstances(t *testing.T) {
	// Create a temp JSONL file.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.jsonl")

	data := `{"instance_id":"django__django-16379","repo":"django/django","base_commit":"abc123","problem_statement":"Fix bug in ORM","hints_text":"Check models.py","version":"4.2"}
{"instance_id":"sympy__sympy-20590","repo":"sympy/sympy","base_commit":"def456","problem_statement":"Fix sympify","hints_text":"","version":"1.9"}
`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	instances, err := LoadSWEBenchInstances(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(instances) != 2 {
		t.Fatalf("expected 2 instances, got %d", len(instances))
	}

	if instances[0].InstanceID != "django__django-16379" {
		t.Errorf("expected django__django-16379, got %s", instances[0].InstanceID)
	}
	if instances[0].Repo != "django/django" {
		t.Errorf("expected django/django, got %s", instances[0].Repo)
	}
	if instances[0].Problem != "Fix bug in ORM" {
		t.Errorf("expected 'Fix bug in ORM', got %q", instances[0].Problem)
	}
	if instances[1].InstanceID != "sympy__sympy-20590" {
		t.Errorf("expected sympy__sympy-20590, got %s", instances[1].InstanceID)
	}
}

func TestLoadSWEBenchInstancesEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	instances, err := LoadSWEBenchInstances(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(instances) != 0 {
		t.Errorf("expected 0 instances, got %d", len(instances))
	}
}

func TestFilterSWEBenchInstances(t *testing.T) {
	instances := []SWEBenchInstance{
		{InstanceID: "django__django-16379", Repo: "django/django"},
		{InstanceID: "sympy__sympy-20590", Repo: "sympy/sympy"},
		{InstanceID: "django__django-16400", Repo: "django/django"},
	}

	t.Run("no_filter", func(t *testing.T) {
		got := FilterSWEBenchInstances(instances, nil, nil)
		if len(got) != 3 {
			t.Errorf("expected 3, got %d", len(got))
		}
	})

	t.Run("by_id", func(t *testing.T) {
		got := FilterSWEBenchInstances(instances, []string{"sympy__sympy-20590"}, nil)
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		if got[0].InstanceID != "sympy__sympy-20590" {
			t.Errorf("expected sympy__sympy-20590, got %s", got[0].InstanceID)
		}
	})

	t.Run("by_repo", func(t *testing.T) {
		got := FilterSWEBenchInstances(instances, nil, []string{"django/django"})
		if len(got) != 2 {
			t.Errorf("expected 2, got %d", len(got))
		}
	})
}

func TestWritePredictions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "predictions.jsonl")

	preds := []SWEBenchPrediction{
		{
			InstanceID:      "django__django-16379",
			ModelNameOrPath: "agentfab-v1",
			ModelPatch:      "diff --git a/foo.py b/foo.py\n--- a/foo.py\n+++ b/foo.py\n@@ -1 +1 @@\n-old\n+new\n",
		},
	}

	if err := WritePredictions(preds, path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read back and verify.
	loaded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	content := string(loaded)
	if !contains(content, "django__django-16379") {
		t.Error("missing instance_id in predictions")
	}
	if !contains(content, "agentfab-v1") {
		t.Error("missing model_name_or_path in predictions")
	}
	if !contains(content, "diff --git") {
		t.Error("missing model_patch in predictions")
	}
}

func TestGenerateSWEBenchReport(t *testing.T) {
	results := []SWEBenchResult{
		{
			InstanceID:   "django__django-16379",
			Repo:         "django/django",
			PatchEmpty:   false,
			EstCostUSD:   1.50,
			Duration:     120_000_000_000, // 2 minutes
			InputTokens:  50000,
			OutputTokens: 10000,
		},
		{
			InstanceID:   "sympy__sympy-20590",
			Repo:         "sympy/sympy",
			PatchEmpty:   true,
			EstCostUSD:   0.80,
			Duration:     60_000_000_000, // 1 minute
			InputTokens:  30000,
			OutputTokens: 5000,
			Error:        "timeout",
		},
	}

	report := GenerateSWEBenchReport(results)

	if !contains(report, "SWE-Bench Results") {
		t.Error("missing title")
	}
	if !contains(report, "Instances attempted") {
		t.Error("missing summary")
	}
	if !contains(report, "django__django-16379") {
		t.Error("missing instance in table")
	}
	if !contains(report, "django/django") {
		t.Error("missing repo in breakdown")
	}
	if !contains(report, "predictions.jsonl") {
		t.Error("missing evaluation instructions")
	}
}

func TestSanitizeInstanceID(t *testing.T) {
	got := sanitizeInstanceID("owner/repo-123")
	if got != "owner__repo-123" {
		t.Errorf("expected 'owner__repo-123', got %q", got)
	}
}

func TestStripHTMLComments(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "single comment",
			in:   "hello <!-- remove me --> world",
			want: "hello  world",
		},
		{
			name: "multiline comment",
			in:   "before\n<!-- line1\nline2\nline3 -->\nafter",
			want: "before\n\nafter",
		},
		{
			name: "multiple comments",
			in:   "<!-- c1 -->text<!-- c2 -->more",
			want: "textmore",
		},
		{
			name: "collapse blank lines",
			in:   "a\n\n<!-- comment -->\n\nb",
			want: "a\n\nb",
		},
		{
			name: "no comments",
			in:   "just plain text",
			want: "just plain text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHTMLComments(tt.in)
			if got != tt.want {
				t.Errorf("stripHTMLComments(%q)\n got: %q\nwant: %q", tt.in, got, tt.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
