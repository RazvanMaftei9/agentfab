package conductor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/local"
)

func TestBuildDecomposeKnowledgeContextEmpty(t *testing.T) {
	if got := decomposeKnowledge(nil, "build todo app"); got != "" {
		t.Fatalf("expected empty context for nil graph, got %q", got)
	}
}

func TestBuildDecomposeKnowledgeContextSanitizesAndCaps(t *testing.T) {
	g := knowledge.NewGraph()
	g.Nodes["designer/md3-mockup"] = &knowledge.Node{
		ID:          "designer/md3-mockup",
		Agent:       "designer",
		Title:       "MD3 <div>Todo</div> `mockup`",
		Summary:     "Use <style> with `surface` tokens and spacing rhythm.\nKeep controls aligned.",
		RequestName: "todo app material design 3",
	}
	g.Nodes["developer/md3-impl"] = &knowledge.Node{
		ID:          "developer/md3-impl",
		Agent:       "developer",
		Title:       "Material 3 implementation notes",
		Summary:     "Implement component states and keep paddings consistent.",
		RequestName: "todo app material design 3",
	}

	got := decomposeKnowledge(g, "update todo app to material design 3")
	if got == "" {
		t.Fatal("expected non-empty context")
	}
	if !strings.Contains(got, "Prior relevant knowledge (shallow lookup):") {
		t.Fatalf("missing shallow lookup header: %q", got)
	}
	if strings.Contains(got, "<div>") || strings.Contains(got, "<style>") || strings.Contains(got, "`") {
		t.Fatalf("expected HTML/markdown markers to be sanitized: %q", got)
	}
}

func TestBuildDecomposeKnowledgeContextRespectsMaxLength(t *testing.T) {
	g := knowledge.NewGraph()
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("n-%02d", i)
		g.Nodes[id] = &knowledge.Node{
			ID:          id,
			Agent:       "architect",
			Title:       fmt.Sprintf("todo material design title %02d", i),
			Summary:     strings.Repeat("very long summary segment ", 30),
			RequestName: "todo material design",
		}
	}

	got := decomposeKnowledge(g, "todo material design refresh")
	if len(got) == 0 {
		t.Fatal("expected non-empty context")
	}
	if len(got) > decomposeContextMaxChars {
		t.Fatalf("context too long: got %d, max %d", len(got), decomposeContextMaxChars)
	}
}

// --- Tests for decomposeDecisions ---

func TestBuildDecomposeDecisionContextEmpty(t *testing.T) {
	if got := decomposeDecisions(nil, ""); got != "" {
		t.Fatalf("expected empty for nil graph, got %q", got)
	}
	if got := decomposeDecisions(knowledge.NewGraph(), ""); got != "" {
		t.Fatalf("expected empty for empty graph, got %q", got)
	}
}

func TestBuildDecomposeDecisionContextFindsDecisions(t *testing.T) {
	g := knowledge.NewGraph()
	g.Nodes["designer/md3-decision"] = &knowledge.Node{
		ID:      "designer/md3-decision",
		Agent:   "designer",
		Title:   "MD3 Design System",
		Summary: "TaskMaster uses Material Design 3 — all UI work must follow MD3 guidelines",
		Tags:    []string{"design", "decision"},
	}
	g.Nodes["architect/rest-api"] = &knowledge.Node{
		ID:      "architect/rest-api",
		Agent:   "architect",
		Title:   "REST API Decision",
		Summary: "REST API with JWT authentication",
		Tags:    []string{"api", "Decision"}, // mixed case
	}
	g.Nodes["developer/impl-notes"] = &knowledge.Node{
		ID:      "developer/impl-notes",
		Agent:   "developer",
		Title:   "Implementation Notes",
		Summary: "Some implementation details",
		Tags:    []string{"implementation"},
	}

	got := decomposeDecisions(g, "")
	if !strings.Contains(got, "Active project decisions:") {
		t.Fatalf("missing header: %q", got)
	}
	if !strings.Contains(got, "[designer]") {
		t.Fatalf("missing designer decision: %q", got)
	}
	if !strings.Contains(got, "[architect]") {
		t.Fatalf("missing architect decision: %q", got)
	}
	if strings.Contains(got, "[developer]") {
		t.Fatalf("non-decision node should not appear: %q", got)
	}
	if !strings.Contains(got, "Material Design 3") {
		t.Fatalf("missing MD3 summary: %q", got)
	}
}

func TestBuildDecomposeDecisionContextFiltersIrrelevant(t *testing.T) {
	g := knowledge.NewGraph()
	g.Nodes["designer/md3-decision"] = &knowledge.Node{
		ID:      "designer/md3-decision",
		Agent:   "designer",
		Title:   "MD3 Design System",
		Summary: "TaskMaster uses Material Design 3",
		Tags:    []string{"design", "decision"},
	}
	g.Nodes["architect/rest-api"] = &knowledge.Node{
		ID:      "architect/rest-api",
		Agent:   "architect",
		Title:   "REST API Decision",
		Summary: "REST API with JWT authentication",
		Tags:    []string{"api", "decision"},
	}

	// Request about "fix syntax error in game engine" — neither decision is relevant.
	got := decomposeDecisions(g, "fix syntax error in game engine")
	if strings.Contains(got, "Material Design") {
		t.Fatalf("irrelevant MD3 decision should be filtered: %q", got)
	}
	if strings.Contains(got, "REST API") {
		t.Fatalf("irrelevant REST API decision should be filtered: %q", got)
	}

	// Request about "update the API" — REST API decision IS relevant.
	got = decomposeDecisions(g, "update the REST API endpoints")
	if !strings.Contains(got, "REST API") {
		t.Fatalf("relevant REST API decision should appear: %q", got)
	}
}

func TestBuildDecomposeDecisionContextSkipsStale(t *testing.T) {
	g := knowledge.NewGraph()
	g.Nodes["designer/old-decision"] = &knowledge.Node{
		ID:        "designer/old-decision",
		Agent:     "designer",
		Title:     "Old Decision",
		Summary:   "This decision is stale",
		Tags:      []string{"decision"},
		TTLDays:   1,
		UpdatedAt: time.Now().Add(-48 * time.Hour), // expired
	}
	g.Nodes["architect/fresh-decision"] = &knowledge.Node{
		ID:      "architect/fresh-decision",
		Agent:   "architect",
		Title:   "Fresh Decision",
		Summary: "This decision is current",
		Tags:    []string{"decision"},
		TTLDays: 0, // no expiry
	}

	got := decomposeDecisions(g, "")
	if strings.Contains(got, "old-decision") || strings.Contains(got, "stale") {
		t.Fatalf("stale decision should not appear: %q", got)
	}
	if !strings.Contains(got, "[architect]") {
		t.Fatalf("fresh decision should appear: %q", got)
	}
}

func TestBuildDecomposeDecisionContextSkipsSuperseded(t *testing.T) {
	g := knowledge.NewGraph()
	g.Nodes["arch/bootstrap"] = &knowledge.Node{
		ID:      "arch/bootstrap",
		Agent:   "architect",
		Title:   "Use Bootstrap",
		Summary: "Bootstrap CSS framework for styling",
		Tags:    []string{"decision", "design-system"},
		TTLDays: 0,
	}
	g.Nodes["arch/md3"] = &knowledge.Node{
		ID:      "arch/md3",
		Agent:   "architect",
		Title:   "Use Material Design 3",
		Summary: "Material Design 3 for all UI components",
		Tags:    []string{"decision", "design-system"},
		TTLDays: 0,
	}
	g.Edges = []*knowledge.Edge{
		{From: "arch/md3", To: "arch/bootstrap", Relation: "supersedes"},
	}

	got := decomposeDecisions(g, "")
	if strings.Contains(got, "Bootstrap") {
		t.Fatalf("superseded decision (Bootstrap) should not appear: %q", got)
	}
	if !strings.Contains(got, "Material Design 3") {
		t.Fatalf("active decision (MD3) should appear: %q", got)
	}
}

func TestBuildDecomposeDecisionContextRespectsMaxChars(t *testing.T) {
	g := knowledge.NewGraph()
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("agent/decision-%02d", i)
		g.Nodes[id] = &knowledge.Node{
			ID:      id,
			Agent:   "architect",
			Title:   fmt.Sprintf("Decision %02d", i),
			Summary: strings.Repeat("established constraint text ", 10),
			Tags:    []string{"decision"},
		}
	}

	got := decomposeDecisions(g, "")
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	if len(got) > decomposeDecisionMaxChars {
		t.Fatalf("output too long: got %d, max %d", len(got), decomposeDecisionMaxChars)
	}
}

// --- Tests for decomposeArtifacts ---

// setupArtifactDir creates a temp base dir with artifact files and returns the path + cleanup func.
func setupArtifactDir(t *testing.T, files map[string]string) string {
	t.Helper()
	base := t.TempDir()
	for relPath, content := range files {
		full := filepath.Join(base, "shared", relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func TestBuildDecomposeArtifactSummaryEmpty(t *testing.T) {
	base := t.TempDir()
	storage := local.NewStorage(base, "conductor")
	got := decomposeArtifacts(storage, "")
	if got != "" {
		t.Fatalf("expected empty for no artifacts, got %q", got)
	}
}

func TestBuildDecomposeArtifactSummaryGroupsByAgent(t *testing.T) {
	base := setupArtifactDir(t, map[string]string{
		"artifacts/developer/todo-app/index.html":  "<html>",
		"artifacts/developer/todo-app/styles.css":  "body{}",
		"artifacts/developer/todo-app/app.js":      "//js",
		"artifacts/architect/docs/api-design.md":   "# API",
		"artifacts/designer/mockups/todo-app.html": "<mock>",
	})
	storage := local.NewStorage(base, "conductor")
	got := decomposeArtifacts(storage, "")

	if !strings.Contains(got, "Existing artifacts on disk:") {
		t.Fatalf("missing header: %q", got)
	}
	if !strings.Contains(got, "developer:") {
		t.Fatalf("missing developer group: %q", got)
	}
	if !strings.Contains(got, "architect:") {
		t.Fatalf("missing architect group: %q", got)
	}
	if !strings.Contains(got, "designer:") {
		t.Fatalf("missing designer group: %q", got)
	}
	if !strings.Contains(got, "todo-app/index.html") {
		t.Fatalf("missing specific file: %q", got)
	}
}

func TestBuildDecomposeArtifactSummaryFiltersNoise(t *testing.T) {
	base := setupArtifactDir(t, map[string]string{
		"artifacts/developer/todo-app/index.html":                "<html>",
		"artifacts/developer/todo-app/node_modules/pkg/index.js": "module",
		"artifacts/_requests/req-1/results.md":                   "results",
		"artifacts/developer/todo-app/.DS_Store":                 "",
		"artifacts/developer/todo-app/package-lock.json":         "{}",
	})
	storage := local.NewStorage(base, "conductor")
	got := decomposeArtifacts(storage, "")

	if strings.Contains(got, "node_modules") {
		t.Fatalf("node_modules should be filtered: %q", got)
	}
	if strings.Contains(got, "_requests") {
		t.Fatalf("_requests should be filtered: %q", got)
	}
	if strings.Contains(got, ".DS_Store") {
		t.Fatalf(".DS_Store should be filtered: %q", got)
	}
	if strings.Contains(got, "package-lock.json") {
		t.Fatalf("package-lock.json should be filtered: %q", got)
	}
	if !strings.Contains(got, "index.html") {
		t.Fatalf("real artifact should remain: %q", got)
	}
}

func TestBuildDecomposeArtifactSummaryRespectsMaxChars(t *testing.T) {
	// Create many files to exceed the limit.
	files := make(map[string]string)
	for i := 0; i < 100; i++ {
		path := fmt.Sprintf("artifacts/developer/app/file-%03d-with-a-long-name-to-fill-space.txt", i)
		files[path] = "content"
	}
	base := setupArtifactDir(t, files)
	storage := local.NewStorage(base, "conductor")
	got := decomposeArtifacts(storage, "")

	if len(got) > decomposeArtifactMaxChars {
		t.Fatalf("output too long: got %d, max %d", len(got), decomposeArtifactMaxChars)
	}
	if got == "" {
		t.Fatal("expected non-empty output")
	}
}
