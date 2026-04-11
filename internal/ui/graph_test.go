package ui

import (
	"strings"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/event"
)

func TestComputeLayers_Linear(t *testing.T) {
	// A → B → C: 3 layers of 1 each.
	tasks := []event.TaskSummary{
		{ID: "A", Agent: "a"},
		{ID: "B", Agent: "b", DependsOn: []string{"A"}},
		{ID: "C", Agent: "c", DependsOn: []string{"B"}},
	}
	layers := computeLayers(tasks)
	if len(layers) != 3 {
		t.Fatalf("expected 3 layers, got %d", len(layers))
	}
	for i, l := range layers {
		if len(l) != 1 {
			t.Errorf("layer %d: expected 1 task, got %d", i, len(l))
		}
	}
	// A=0, B=1, C=2
	if layers[0][0] != 0 {
		t.Errorf("expected A (idx 0) in layer 0, got idx %d", layers[0][0])
	}
	if layers[1][0] != 1 {
		t.Errorf("expected B (idx 1) in layer 1, got idx %d", layers[1][0])
	}
	if layers[2][0] != 2 {
		t.Errorf("expected C (idx 2) in layer 2, got idx %d", layers[2][0])
	}
}

func TestComputeLayers_Diamond(t *testing.T) {
	// A → {B, C} → D: 3 layers, middle has 2.
	tasks := []event.TaskSummary{
		{ID: "A", Agent: "a"},
		{ID: "B", Agent: "b", DependsOn: []string{"A"}},
		{ID: "C", Agent: "c", DependsOn: []string{"A"}},
		{ID: "D", Agent: "d", DependsOn: []string{"B", "C"}},
	}
	layers := computeLayers(tasks)
	if len(layers) != 3 {
		t.Fatalf("expected 3 layers, got %d", len(layers))
	}
	if len(layers[0]) != 1 {
		t.Errorf("layer 0: expected 1 task, got %d", len(layers[0]))
	}
	if len(layers[1]) != 2 {
		t.Errorf("layer 1: expected 2 tasks, got %d", len(layers[1]))
	}
	if len(layers[2]) != 1 {
		t.Errorf("layer 2: expected 1 task, got %d", len(layers[2]))
	}
}

func TestComputeLayers_SingleTask(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "dev"},
	}
	layers := computeLayers(tasks)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer, got %d", len(layers))
	}
	if len(layers[0]) != 1 {
		t.Errorf("expected 1 task in layer 0, got %d", len(layers[0]))
	}
}

func TestComputeLayers_TwoParallel(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "A", Agent: "a"},
		{ID: "B", Agent: "b"},
	}
	layers := computeLayers(tasks)
	if len(layers) != 1 {
		t.Fatalf("expected 1 layer (all parallel), got %d", len(layers))
	}
	if len(layers[0]) != 2 {
		t.Errorf("expected 2 tasks in layer 0, got %d", len(layers[0]))
	}
}

func TestComputeLayers_Empty(t *testing.T) {
	layers := computeLayers(nil)
	if layers != nil {
		t.Errorf("expected nil for empty tasks, got %v", layers)
	}
}

func TestLayoutDAG_FitsWidth(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "designer", Status: "completed"},
		{ID: "T2", Agent: "developer", Status: "running", DependsOn: []string{"T1"}},
	}
	layout := layoutDAG(tasks, 120)
	if layout == nil {
		t.Fatal("expected layout to fit in 120 columns")
	}
	if layout.totalW > 120 {
		t.Errorf("layout width %d exceeds terminal width 120", layout.totalW)
	}
	if len(layout.layers) != 2 {
		t.Errorf("expected 2 layers, got %d", len(layout.layers))
	}
}

func TestLayoutDAG_TooNarrow(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "designer"},
		{ID: "T2", Agent: "developer", DependsOn: []string{"T1"}},
	}
	layout := layoutDAG(tasks, 20) // way too narrow for 2 layers
	if layout != nil {
		t.Errorf("expected nil layout for narrow terminal, got totalW=%d", layout.totalW)
	}
}

func TestTaskAgentLabelIncludesExecutionNode(t *testing.T) {
	task := event.TaskSummary{Agent: "designer", ExecutionNode: "node-2"}
	if got := taskAgentLabel(task); got != "designer@node-2" {
		t.Fatalf("taskAgentLabel() = %q, want designer@node-2", got)
	}
}

func TestRenderDAG_StatusColors(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "designer", Status: "completed"},
		{ID: "T2", Agent: "developer", Status: "running", DependsOn: []string{"T1"}},
		{ID: "T3", Agent: "reviewer", Status: "pending", DependsOn: []string{"T2"}},
	}
	layout := layoutDAG(tasks, 120)
	if layout == nil {
		t.Fatal("expected layout to fit")
	}

	colorFn := func(agent string) string { return Cyan }
	lines := renderDAG(layout, tasks, nil, 0, true, colorFn)
	if len(lines) == 0 {
		t.Fatal("expected non-empty render output")
	}

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, Cyan) {
		t.Error("expected Cyan color for completed task")
	}
	if !strings.Contains(joined, Cyan) {
		t.Error("expected agent color (Cyan) for running task")
	}
	if !strings.Contains(joined, Gray) {
		t.Error("expected Gray color for pending task")
	}
}

func TestRenderDAG_NonTTY(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "dev", Status: "pending"},
	}
	layout := layoutDAG(tasks, 80)
	if layout == nil {
		t.Fatal("expected layout to fit")
	}

	lines := renderDAG(layout, tasks, nil, 0, false, nil)
	joined := strings.Join(lines, "\n")
	// Non-TTY should not have ANSI codes.
	if strings.Contains(joined, "\033[") {
		t.Error("non-TTY output should not contain ANSI escape codes")
	}
	if !strings.Contains(joined, "T1") {
		t.Error("expected task ID T1 in output")
	}
}

func TestRenderDAG_NilLayout(t *testing.T) {
	lines := renderDAG(nil, nil, nil, 0, true, nil)
	if lines != nil {
		t.Errorf("expected nil for nil layout, got %d lines", len(lines))
	}
}

func TestBoxChar_AllDirections(t *testing.T) {
	tests := []struct {
		dirs uint8
		want string
	}{
		{DirLeft | DirRight, "─"},
		{DirUp | DirDown, "│"},
		{DirRight | DirDown, ConnTopLeft},
		{DirLeft | DirDown, ConnTopRight},
		{DirRight | DirUp, ConnBotLeft},
		{DirLeft | DirUp, ConnBotRight},
		{DirRight | DirUp | DirDown, ConnTeeRight},
		{DirLeft | DirUp | DirDown, ConnTeeLeft},
		{DirLeft | DirRight | DirDown, ConnTeeDown},
		{DirLeft | DirRight | DirUp, ConnTeeUp},
		{DirLeft | DirRight | DirUp | DirDown, ConnCross},
		{DirRight, "─"},
		{DirLeft, ConnArrow},
		{DirUp, "│"},
		{DirDown, "│"},
		{0, " "},
	}
	for _, tt := range tests {
		got := boxChar(tt.dirs)
		if got != tt.want {
			t.Errorf("boxChar(%08b) = %q, want %q", tt.dirs, got, tt.want)
		}
	}
}

func TestRenderConnectorColumn(t *testing.T) {
	// Diamond: A → {B, C}, both in same connector column.
	tasks := []event.TaskSummary{
		{ID: "A", Agent: "a", Status: "completed"},
		{ID: "B", Agent: "b", Status: "running", DependsOn: []string{"A"}},
		{ID: "C", Agent: "c", Status: "pending", DependsOn: []string{"A"}},
	}
	layout := layoutDAG(tasks, 120)
	if layout == nil {
		t.Fatal("expected layout to fit")
	}

	colorFn := func(agent string) string { return "" }
	lines := renderDAG(layout, tasks, nil, 0, false, colorFn)
	if len(lines) == 0 {
		t.Fatal("expected non-empty render output")
	}

	// Should have connector characters somewhere.
	joined := strings.Join(lines, "\n")
	hasConnector := false
	for _, ch := range []string{"─", "│", ConnTeeRight, ConnTopLeft, ConnBotLeft, ConnArrow} {
		if strings.Contains(joined, ch) {
			hasConnector = true
			break
		}
	}
	if !hasConnector {
		t.Error("expected at least one connector character in output")
	}
}

func TestRenderDAG_RunningFlash(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "dev", Status: "running"},
	}
	layout := layoutDAG(tasks, 80)
	if layout == nil {
		t.Fatal("expected layout to fit")
	}

	colorFn := func(agent string) string { return Cyan }

	// Frame 0: should be bright agent color (0 % 4 = 0 < 2).
	lines0 := renderDAG(layout, tasks, nil, 0, true, colorFn)
	joined0 := strings.Join(lines0, "\n")

	// Frame 2: should be Dim+agent color (2 % 4 = 2, not < 2).
	lines2 := renderDAG(layout, tasks, nil, 2, true, colorFn)
	joined2 := strings.Join(lines2, "\n")

	if !strings.Contains(joined0, Cyan) {
		t.Error("frame 0: expected agent color (Cyan) for running task")
	}
	if !strings.Contains(joined2, Dim) {
		t.Error("frame 2: expected Dim color for running task flash")
	}

	// Frame 1 should also be bright (1 % 4 = 1 < 2).
	lines1 := renderDAG(layout, tasks, nil, 1, true, colorFn)
	joined1 := strings.Join(lines1, "\n")
	if !strings.Contains(joined1, Cyan) {
		t.Error("frame 1: expected agent color (Cyan) for running task")
	}
}

func TestNodeBoxWidth(t *testing.T) {
	tests := []struct {
		name string
		task event.TaskSummary
		minW int
	}{
		{"short", event.TaskSummary{ID: "T1", Agent: "dev"}, boxMinW},
		{"long agent", event.TaskSummary{ID: "T1", Agent: "infrastructure-specialist"}, 14},
		{"long ID", event.TaskSummary{ID: "very-long-task-id", Agent: "dev"}, 14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := nodeBoxWidth(tt.task)
			if w < tt.minW {
				t.Errorf("nodeBoxWidth() = %d, want >= %d", w, tt.minW)
			}
			if w < boxMinW {
				t.Errorf("nodeBoxWidth() = %d, below minimum %d", w, boxMinW)
			}
		})
	}
}

func TestRenderSnippetBar(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "dev", Status: "running"},
		{ID: "T2", Agent: "designer", Status: "pending"},
	}
	progress := map[string]string{
		"dev": "implementing the user auth flow with JWT tokens",
	}
	colorFn := func(agent string) string { return Cyan }

	lines := renderSnippetBar(tasks, progress, 0, 100, true, colorFn)
	if len(lines) != 1 {
		t.Fatalf("expected 1 snippet line (only running task with progress), got %d", len(lines))
	}
	if !strings.Contains(lines[0], "dev") {
		t.Error("expected agent name in snippet line")
	}
	if !strings.Contains(lines[0], "auth flow") {
		t.Error("expected snippet text in output")
	}
}

func TestRenderSnippetBar_NonTTY(t *testing.T) {
	tasks := []event.TaskSummary{
		{ID: "T1", Agent: "dev", Status: "running"},
	}
	progress := map[string]string{"dev": "working..."}

	lines := renderSnippetBar(tasks, progress, 0, 100, false, nil)
	if len(lines) != 0 {
		t.Errorf("expected no snippet lines for non-TTY, got %d", len(lines))
	}
}

func TestPadRight(t *testing.T) {
	tests := []struct {
		s    string
		w    int
		want string
	}{
		{"abc", 6, "abc   "},
		{"abc", 3, "abc"},
		{"abcdef", 3, "abc"},
	}
	for _, tt := range tests {
		got := padRight(tt.s, tt.w)
		if got != tt.want {
			t.Errorf("padRight(%q, %d) = %q, want %q", tt.s, tt.w, got, tt.want)
		}
	}
}

func TestNodeColor(t *testing.T) {
	tests := []struct {
		status     string
		frame      int
		tty        bool
		agentColor string
		want       string
	}{
		{"completed", 0, true, Yellow, Yellow},
		{"running", 0, true, Yellow, Yellow},
		{"running", 2, true, Yellow, Dim + Yellow},
		{"failed", 0, true, Yellow, Red},
		{"pending", 0, true, Yellow, Gray},
		{"cancelled", 0, true, Yellow, Gray},
		{"pending", 0, false, Yellow, ""},
	}
	for _, tt := range tests {
		got := nodeColor(tt.status, tt.frame, tt.tty, tt.agentColor)
		if got != tt.want {
			t.Errorf("nodeColor(%q, %d, %v, %q) = %q, want %q", tt.status, tt.frame, tt.tty, tt.agentColor, got, tt.want)
		}
	}
}
