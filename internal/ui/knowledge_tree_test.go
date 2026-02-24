package ui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/razvanmaftei/agentfab/internal/event"
)

// --- Tree building tests ---

func TestBuildKnowledgeTree_OwnOnly(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/auth-impl", Agent: "developer", Title: "Auth Implementation"},
		{ID: "dev/db-layer", Agent: "developer", Title: "Database Layer"},
		{ID: "dev/ui-theme", Agent: "developer", Title: "UI Theme"},
	}
	tree := buildKnowledgeTree(own, nil, nil)
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	if tree.root == nil {
		t.Fatal("expected non-nil root")
	}
	if len(tree.root.children) != 3 {
		t.Errorf("expected 3 children, got %d", len(tree.root.children))
	}
	// All children should be own nodes.
	for _, c := range tree.root.children {
		if !c.isOwn {
			t.Errorf("expected own node, got related for %s", c.id)
		}
	}
	// Total allNodes = root + 3 children.
	if len(tree.allNodes) != 4 {
		t.Errorf("expected 4 allNodes, got %d", len(tree.allNodes))
	}
}

func TestBuildKnowledgeTree_OwnAndRelated(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/auth-impl", Agent: "developer", Title: "Auth Implementation"},
	}
	related := []event.KnowledgeRelInfo{
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "arch/api-design", Agent: "architect", Title: "API Design"},
			Depth:             1,
			Relations:         []string{"implements"},
		},
	}
	edges := []event.KnowledgeEdgeInfo{
		{From: "dev/auth-impl", To: "arch/api-design", Relation: "implements"},
	}
	tree := buildKnowledgeTree(own, related, edges)
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	// auth-impl is child of root, api-design is child of auth-impl.
	if len(tree.root.children) != 1 {
		t.Fatalf("expected 1 root child, got %d", len(tree.root.children))
	}
	authNode := tree.root.children[0]
	if authNode.id != "dev/auth-impl" {
		t.Errorf("expected dev/auth-impl, got %s", authNode.id)
	}
	if len(authNode.children) != 1 {
		t.Fatalf("expected 1 child of auth-impl, got %d", len(authNode.children))
	}
	if authNode.children[0].id != "arch/api-design" {
		t.Errorf("expected arch/api-design, got %s", authNode.children[0].id)
	}
}

func TestBuildKnowledgeTree_DeduplicatesMultiplePaths(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/a", Agent: "developer", Title: "A"},
		{ID: "dev/b", Agent: "developer", Title: "B"},
	}
	related := []event.KnowledgeRelInfo{
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "dev/c", Agent: "developer", Title: "C"},
			Depth:             1,
			Relations:         []string{"depends_on"},
		},
	}
	// c is reachable from both a and b.
	edges := []event.KnowledgeEdgeInfo{
		{From: "dev/a", To: "dev/c", Relation: "depends_on"},
		{From: "dev/b", To: "dev/c", Relation: "depends_on"},
	}
	tree := buildKnowledgeTree(own, related, edges)
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	// c should appear only once.
	count := 0
	for _, n := range tree.allNodes {
		if n.id == "dev/c" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected dev/c to appear once, appeared %d times", count)
	}
}

func TestBuildKnowledgeTree_EmptyResult(t *testing.T) {
	tree := buildKnowledgeTree(nil, nil, nil)
	if tree != nil {
		t.Errorf("expected nil tree for empty input, got non-nil")
	}
}

func TestBuildKnowledgeTree_RelatedNoEdges(t *testing.T) {
	related := []event.KnowledgeRelInfo{
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "arch/x", Agent: "architect", Title: "X"},
			Depth:             0,
			Relations:         []string{"decision"},
		},
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "arch/y", Agent: "architect", Title: "Y"},
			Depth:             0,
			Relations:         []string{"decision"},
		},
	}
	tree := buildKnowledgeTree(nil, related, nil)
	if tree == nil {
		t.Fatal("expected non-nil tree")
	}
	// Both should be attached to root (no edges to connect them elsewhere).
	if len(tree.root.children) != 2 {
		t.Errorf("expected 2 root children, got %d", len(tree.root.children))
	}
}

// --- Rendering tests ---

func TestRenderKnowledgeTree_FullTree(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/auth-impl", Agent: "developer", Title: "auth-impl"},
		{ID: "dev/db-layer", Agent: "developer", Title: "db-layer"},
	}
	related := []event.KnowledgeRelInfo{
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "arch/api-design", Agent: "architect", Title: "api-design"},
			Depth:             1,
			Relations:         []string{"implements"},
		},
	}
	edges := []event.KnowledgeEdgeInfo{
		{From: "dev/auth-impl", To: "arch/api-design", Relation: "implements"},
	}
	tree := buildKnowledgeTree(own, related, edges)

	noColor := func(agent string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 120, false, noColor)
	if lines == nil {
		t.Fatal("expected non-nil lines for width=120")
	}
	joined := strings.Join(lines, "\n")
	// Should contain the root symbol.
	if !strings.Contains(joined, "◆") {
		t.Error("expected root symbol ◆ in output")
	}
	if !strings.Contains(joined, "├──") && !strings.Contains(joined, "└──") {
		t.Error("expected branch connectors in arborescent output")
	}
	if !strings.Contains(joined, "●") {
		t.Error("expected node markers in output")
	}
	// Should contain node labels.
	if !strings.Contains(joined, "auth-impl") {
		t.Error("expected 'auth-impl' in output")
	}
	if !strings.Contains(joined, "db-layer") {
		t.Error("expected 'db-layer' in output")
	}
	if !strings.Contains(joined, "api-design") {
		t.Error("expected 'api-design' in output")
	}
	if strings.Contains(joined, "◆ knowledge") {
		t.Error("did not expect duplicated 'knowledge' root label")
	}
}

func TestRenderKnowledgeTree_Miniature(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/a", Agent: "developer", Title: "A"},
		{ID: "dev/b", Agent: "developer", Title: "B"},
	}
	tree := buildKnowledgeTree(own, nil, nil)

	noColor := func(agent string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 20, false, noColor)
	if lines == nil {
		t.Fatal("expected non-nil lines for miniature mode")
	}
	joined := strings.Join(lines, "\n")
	// Should contain root symbol and dot placeholders.
	if !strings.Contains(joined, "◆") {
		t.Error("expected root symbol ◆ in miniature output")
	}
	if !strings.Contains(joined, "●") {
		t.Error("expected dot ● in miniature output")
	}
}

func TestRenderKnowledgeTree_TooNarrow(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/a", Agent: "developer", Title: "A"},
	}
	tree := buildKnowledgeTree(own, nil, nil)

	noColor := func(agent string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 10, false, noColor)
	if lines != nil {
		t.Errorf("expected nil for width=10, got %d lines", len(lines))
	}
}

func TestRenderKnowledgeTree_FlashAnimation(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/a", Agent: "developer", Title: "Test Node"},
	}
	tree := buildKnowledgeTree(own, nil, nil)

	colorFn := func(agent string) string { return Blue }

	lines0 := renderKnowledgeTreeLines(tree, "developer", 0, 80, true, colorFn)
	lines2 := renderKnowledgeTreeLines(tree, "developer", 2, 80, true, colorFn)

	if lines0 == nil || lines2 == nil {
		t.Fatal("expected non-nil lines for both frames")
	}

	joined0 := strings.Join(lines0, "\n")
	joined2 := strings.Join(lines2, "\n")

	// Frame 0 (0%4=0 < 2) should use full color, frame 2 (2%4=2 >= 2) should use Dim+color.
	// They should differ because of the flash.
	if joined0 == joined2 {
		t.Error("expected different output between frame 0 and frame 2 (flash animation)")
	}
}

func TestFlashColor_UsesLookupAgentColor(t *testing.T) {
	colorFn := func(agent string) string {
		if agent == "developer" {
			return Blue
		}
		return Red
	}

	got := flashColor(0, true, colorFn, "developer")
	if got != Blue {
		t.Fatalf("expected lookup agent color %q, got %q", Blue, got)
	}
}

func TestRenderKnowledgeTree_NonTTY(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/auth", Agent: "developer", Title: "Auth"},
	}
	tree := buildKnowledgeTree(own, nil, nil)

	noColor := func(agent string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 80, false, noColor)
	if lines == nil {
		t.Fatal("expected non-nil lines for non-TTY")
	}
	joined := strings.Join(lines, "\n")
	// Should not contain ANSI escape codes.
	if strings.Contains(joined, "\033[") {
		t.Error("expected no ANSI codes in non-TTY output")
	}
}

func TestRenderKnowledgeTree_SingleNode(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/only-one", Agent: "developer", Title: "Only One"},
	}
	tree := buildKnowledgeTree(own, nil, nil)

	noColor := func(agent string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 80, false, noColor)
	if lines == nil {
		t.Fatal("expected non-nil lines for single node")
	}
	if len(lines) != 1 {
		t.Errorf("expected 1 line for single node, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "◆") {
		t.Error("expected root symbol in single-node output")
	}
	if !strings.Contains(lines[0], "Only One") {
		t.Error("expected 'Only One' in single-node output")
	}
}

func TestRenderKnowledgeTree_Truncated(t *testing.T) {
	own := []event.KnowledgeNodeInfo{
		{ID: "dev/auth-impl", Agent: "developer", Title: "auth-impl"},
		{ID: "dev/db-layer", Agent: "developer", Title: "db-layer"},
		{ID: "dev/ui-theme", Agent: "developer", Title: "ui-theme"},
	}
	related := []event.KnowledgeRelInfo{
		{
			KnowledgeNodeInfo: event.KnowledgeNodeInfo{ID: "arch/api-design", Agent: "architect", Title: "api-design-very-long-name"},
			Depth:             1,
			Relations:         []string{"implements"},
		},
	}
	tree := buildKnowledgeTree(own, related, nil)

	noColor := func(agent string) string { return "" }
	// Use a width that's too narrow for full but enough for truncated.
	lines := renderKnowledgeTreeLines(tree, "developer", 0, 40, false, noColor)
	if lines == nil {
		t.Fatal("expected non-nil lines for truncated mode")
	}
	// Should still have the root symbol.
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "◆") {
		t.Error("expected root symbol in truncated output")
	}
}

// --- Helper tests ---

func TestNodeLabel(t *testing.T) {
	tests := []struct {
		id, title, want string
	}{
		{"dev/auth-impl", "Auth Implementation", "Auth Implementation"},
		{"dev/auth-impl", "", "auth-impl"},
		{"standalone", "", "standalone"},
		{"dev/auth-impl", "Custom Title", "Custom Title"},
	}
	for _, tt := range tests {
		got := nodeLabel(tt.id, tt.title)
		if got != tt.want {
			t.Errorf("nodeLabel(%q, %q) = %q, want %q", tt.id, tt.title, got, tt.want)
		}
	}
}

func TestTruncateLabel(t *testing.T) {
	tests := []struct {
		label    string
		maxWidth int
		want     string
	}{
		{"short", 10, "short"},
		{"this is a very long label", 10, "this is..."},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"", 5, ""},
	}
	for _, tt := range tests {
		got := truncateLabel(tt.label, tt.maxWidth)
		if got != tt.want {
			t.Errorf("truncateLabel(%q, %d) = %q, want %q", tt.label, tt.maxWidth, got, tt.want)
		}
	}
}

func TestPurposeLabel_SanitizesAndTruncates(t *testing.T) {
	in := "Can you add a calendar feature\n\nQ: What should this do? For example, support recurring events and reminders."
	got := purposeLabel(in)
	if strings.Contains(got, "\n") {
		t.Fatalf("expected no newlines, got %q", got)
	}
	if strings.Contains(got, "Q:") {
		t.Fatalf("expected question spillover to be removed, got %q", got)
	}
	if utf8.RuneCountInString(got) > maxKnowledgeLabelRunes {
		t.Fatalf("expected truncated label <= %d runes, got %d (%q)", maxKnowledgeLabelRunes, utf8.RuneCountInString(got), got)
	}
}

func TestNormalizeKnowledgeTreeLines_DedupsRoot(t *testing.T) {
	lines := []string{
		"    ◆ knowledge",
		"    ◆",
		"    ├── ○ Node A",
	}
	got := normalizeKnowledgeTreeLines(lines)
	if len(got) != 2 {
		t.Fatalf("expected 2 lines after root dedup, got %d (%v)", len(got), got)
	}
	if strings.TrimSpace(got[0]) != "◆" {
		t.Fatalf("expected canonical root line, got %q", got[0])
	}
}

func TestColorWrap_NonTTY(t *testing.T) {
	got := colorWrap("hello", Blue, false)
	if got != "hello" {
		t.Errorf("expected plain text in non-TTY, got %q", got)
	}
}

func TestColorWrap_TTY(t *testing.T) {
	got := colorWrap("hello", Blue, true)
	if !strings.HasPrefix(got, Blue) || !strings.HasSuffix(got, Reset) {
		t.Errorf("expected color wrapped text, got %q", got)
	}
}
