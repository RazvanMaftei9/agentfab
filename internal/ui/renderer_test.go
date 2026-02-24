package ui

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/razvanmaftei/agentfab/internal/event"
)

func TestRenderStartupTTY(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.AgentReady, AgentName: "planner", AgentModel: "anthropic/claude-sonnet-4"})
		bus.Emit(event.Event{Type: event.AgentReady, AgentName: "developer", AgentModel: "anthropic/claude-sonnet-4"})
		bus.Emit(event.Event{Type: event.AllAgentsReady})
	}()

	r.RenderStartup(bus, "my-project", 2)
	out := buf.String()

	if !strings.Contains(out, "agentfab") {
		t.Error("expected header with lowercase agentfab")
	}
	if !strings.Contains(out, BoxHorizontal) {
		t.Error("expected box-drawing characters in header")
	}
	if !strings.Contains(out, "●") {
		t.Error("expected colored dot for agents")
	}
	if !strings.Contains(out, "planner") {
		t.Error("expected planner agent")
	}
	if !strings.Contains(out, "developer") {
		t.Error("expected developer agent")
	}
	if !strings.Contains(out, "2 agents ready.") {
		t.Error("expected agent count in ready message")
	}
}

func TestRenderStartupNonTTY(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.AgentReady, AgentName: "dev", AgentModel: "openai/gpt-4"})
		bus.Emit(event.Event{Type: event.AllAgentsReady})
	}()

	r.RenderStartup(bus, "test", 1)
	out := buf.String()

	if strings.Contains(out, "\033[") {
		t.Error("non-TTY output should not contain ANSI codes")
	}
	if !strings.Contains(out, "OK dev") {
		t.Error("expected OK prefix for agent")
	}
	if !strings.Contains(out, "agentfab v0.1.0 -- test") {
		t.Error("expected plain header")
	}
	if !strings.Contains(out, "1 agents ready.") {
		t.Error("expected agent count")
	}
}

func TestRenderRequestLifecycle(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false) // non-TTY for predictable output

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type: event.DecomposeEnd,
			Tasks: []event.TaskSummary{
				{ID: "t1", Agent: "planner", Status: "pending"},
				{ID: "t2", Agent: "developer", Status: "pending"},
			},
			InputTokens:  1500,
			OutputTokens: 300,
			TotalCalls:   1,
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			AgentInputTokens:  2000,
			AgentOutputTokens: 500,
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t2"})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t2",
			AgentInputTokens:  3000,
			AgentOutputTokens: 800,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 2 * time.Second,
			InputTokens:   6500,
			OutputTokens:  1600,
			TotalCalls:    3,
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if !strings.Contains(out, "Decomposing") {
		t.Error("expected decomposing message")
	}
	if !strings.Contains(out, "--- Decomposition ---") {
		t.Error("expected Decomposition section header")
	}
	if !strings.Contains(out, "--- Execution ---") {
		t.Error("expected Execution section header")
	}
	if !strings.Contains(out, "2 tasks planned") {
		t.Error("expected task plan summary")
	}
	if !strings.Contains(out, "t1") {
		t.Error("expected task t1 in output")
	}
	if !strings.Contains(out, "Done") {
		t.Error("expected Done summary")
	}
	if !strings.Contains(out, "8,100 tokens") {
		t.Errorf("expected total tokens in summary, got: %s", out)
	}
}

func TestRenderRequestWithFailures(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{Type: event.TaskFailed, TaskID: "t1", ErrMsg: "timeout"})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 1 * time.Second,
			HasFailures:   true,
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if !strings.Contains(out, "errors") {
		t.Error("expected 'errors' in summary when tasks fail")
	}
}

func TestRenderRequestZeroTasks(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{Type: event.DecomposeEnd})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 100 * time.Millisecond,
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if !strings.Contains(out, "Done") {
		t.Error("expected Done even with zero tasks")
	}
}

func TestRenderStartupClosedBus(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	bus.Close()

	// Should not hang — range over closed channel exits.
	r.RenderStartup(bus, "test", 0)
}

func TestRenderRequestShowsTokens(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:         event.DecomposeEnd,
			Tasks:        []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
			InputTokens:  1234,
			OutputTokens: 567,
			TotalCalls:   1,
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			AgentInputTokens:  4500,
			AgentOutputTokens: 1200,
			InputTokens:       5734,
			OutputTokens:      1767,
			TotalCalls:        2,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 3 * time.Second,
			InputTokens:   5734,
			OutputTokens:  1767,
			TotalCalls:    2,
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	// Decompose token line.
	if !strings.Contains(out, "1,234 in") {
		t.Errorf("expected decompose input tokens, got: %s", out)
	}
	if !strings.Contains(out, "567 out") {
		t.Errorf("expected decompose output tokens, got: %s", out)
	}
	// Summary token line.
	if !strings.Contains(out, "7,501 tokens") {
		t.Errorf("expected total tokens in summary, got: %s", out)
	}
	if !strings.Contains(out, "2 calls") {
		t.Errorf("expected call count in summary, got: %s", out)
	}
}

func TestRenderRequestLoopTransition(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:      event.LoopTransition,
			TaskID:    "t1",
			LoopID:    "loop-1",
			FromState: "WORKING",
			ToState:   "REVIEWING",
			Verdict:   "",
			LoopCount: 1,
		})
		bus.Emit(event.Event{
			Type:      event.LoopTransition,
			TaskID:    "t1",
			LoopID:    "loop-1",
			FromState: "REVIEWING",
			ToState:   "APPROVED",
			Verdict:   "approved",
			LoopCount: 2,
		})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			AgentInputTokens:  5000,
			AgentOutputTokens: 2000,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 5 * time.Second,
		})
	}()

	r.RenderRequest(bus)
	out := buf.String()

	if !strings.Contains(out, "LOOP loop-1") {
		t.Errorf("expected loop transition line, got: %s", out)
	}
	if !strings.Contains(out, "WORKING -> REVIEWING") {
		t.Errorf("expected state transition, got: %s", out)
	}
	if !strings.Contains(out, "verdict: approved") {
		t.Errorf("expected verdict in loop transition, got: %s", out)
	}
}

func TestRenderRequestTTYClearsStaleGraphFrames(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{
			Type: event.DecomposeEnd,
			Tasks: []event.TaskSummary{
				{ID: "t1", Agent: "architect", Status: "pending"},
				{ID: "t2", Agent: "designer", Status: "pending", DependsOn: []string{"t1"}},
				{ID: "t4", Agent: "developer", Status: "pending", DependsOn: []string{"t2"}},
			},
		})
		bus.Emit(event.Event{
			Type:            event.TaskStart,
			TaskID:          "t1",
			TaskAgent:       "architect",
			TaskDescription: "Design the architecture",
		})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			TaskAgent:         "architect",
			ResultSummary:     "Completed architecture design",
			AgentInputTokens:  100,
			AgentOutputTokens: 50,
		})
		bus.Emit(event.Event{
			Type:            event.TaskStart,
			TaskID:          "t2",
			TaskAgent:       "designer",
			TaskDescription: "Create the UI",
		})
		bus.Emit(event.Event{Type: event.RequestComplete})
	}()

	r.RenderRequest(bus)
	screen := emulateTerminalScreen(buf.String())

	if strings.Contains(screen, "t1\narchitect\n· working...") {
		t.Fatalf("stale running frame survived task completion:\n%s", screen)
	}
	if !strings.Contains(screen, "t2") || !strings.Contains(screen, "designer") {
		t.Fatalf("expected updated graph content in final screen:\n%s", screen)
	}
	if !strings.Contains(screen, "✓ t1") {
		t.Fatalf("expected completion event line in final screen:\n%s", screen)
	}
}

func TestRenderRequestTTYClearsGraphAfterKnowledgeLookup(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{
			Type: event.DecomposeEnd,
			Tasks: []event.TaskSummary{
				{ID: "t1", Agent: "architect", Status: "pending"},
				{ID: "t2", Agent: "designer", Status: "pending", DependsOn: []string{"t1"}},
				{ID: "t4", Agent: "developer", Status: "pending", DependsOn: []string{"t2"}},
			},
		})
		bus.Emit(event.Event{
			Type:            event.TaskStart,
			TaskID:          "t1",
			TaskAgent:       "architect",
			TaskDescription: "Design the complete system architecture",
		})
		bus.Emit(event.Event{
			Type:                 event.KnowledgeLookup,
			KnowledgeLookupAgent: "architect",
			KnowledgeLookupOwnNodes: []event.KnowledgeNodeInfo{
				{ID: "n1", Agent: "architect", Title: "Component Structure and Hierarchy"},
				{ID: "n2", Agent: "architect", Title: "Technology Stack Decision"},
			},
		})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			TaskAgent:         "architect",
			ResultSummary:     "Completed architecture design",
			AgentInputTokens:  100,
			AgentOutputTokens: 50,
		})
		bus.Emit(event.Event{
			Type:            event.TaskStart,
			TaskID:          "t2",
			TaskAgent:       "designer",
			TaskDescription: "Create the UI",
		})
		bus.Emit(event.Event{
			Type:                 event.KnowledgeLookup,
			KnowledgeLookupAgent: "designer",
			KnowledgeLookupOwnNodes: []event.KnowledgeNodeInfo{
				{ID: "n3", Agent: "designer", Title: "Date and Time Picker UI Design"},
			},
		})
		bus.Emit(event.Event{Type: event.RequestComplete})
	}()

	r.RenderRequest(bus)
	screen := emulateTerminalScreen(buf.String())

	if strings.Contains(screen, "t1\narchitect\n· working...") {
		t.Fatalf("stale graph frame remained after knowledge lookup redraw:\n%s", screen)
	}
	if strings.Contains(screen, "architect working...") && strings.Contains(screen, "✓ t1") {
		t.Fatalf("completed task still appears as running in final screen:\n%s", screen)
	}
	if !strings.Contains(screen, "designer reading 1 knowledge nodes") {
		t.Fatalf("expected knowledge lookup output in final screen:\n%s", screen)
	}
}

func emulateTerminalScreen(out string) string {
	var screen [][]rune
	row := 0
	col := 0

	ensureRow := func(target int) {
		for len(screen) <= target {
			screen = append(screen, []rune{})
		}
	}
	ensureCol := func(targetRow, targetCol int) {
		ensureRow(targetRow)
		for len(screen[targetRow]) <= targetCol {
			screen[targetRow] = append(screen[targetRow], ' ')
		}
	}
	clearDown := func(fromRow, fromCol int) {
		if fromRow >= len(screen) {
			return
		}
		ensureCol(fromRow, fromCol)
		for c := fromCol; c < len(screen[fromRow]); c++ {
			screen[fromRow][c] = ' '
		}
		for r := fromRow + 1; r < len(screen); r++ {
			for c := range screen[r] {
				screen[r][c] = ' '
			}
		}
	}
	clearLine := func(targetRow int) {
		if targetRow >= len(screen) {
			return
		}
		for c := range screen[targetRow] {
			screen[targetRow][c] = ' '
		}
	}

	for i := 0; i < len(out); {
		switch out[i] {
		case '\n':
			row++
			col = 0
			ensureRow(row)
			i++
		case '\r':
			col = 0
			i++
		case 0x1b:
			if i+1 >= len(out) || out[i+1] != '[' {
				i++
				continue
			}
			j := i + 2
			for j < len(out) && ((out[j] >= '0' && out[j] <= '9') || out[j] == ';') {
				j++
			}
			if j >= len(out) {
				i = j
				continue
			}
			params := out[i+2 : j]
			switch out[j] {
			case 'A':
				n := 1
				if params != "" {
					if parsed, err := strconv.Atoi(params); err == nil && parsed > 0 {
						n = parsed
					}
				}
				row -= n
				if row < 0 {
					row = 0
				}
			case 'J':
				clearDown(row, col)
			case 'K':
				clearLine(row)
				col = 0
			case 'm':
			}
			i = j + 1
		default:
			rn, size := utf8.DecodeRuneInString(out[i:])
			ensureCol(row, col)
			screen[row][col] = rn
			col++
			i += size
		}
	}

	lines := make([]string, 0, len(screen))
	for _, line := range screen {
		lines = append(lines, strings.TrimRight(string(line), " "))
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

func TestRenderRequestWithSummaries(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false) // non-TTY for predictable output

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type: event.DecomposeEnd,
			Tasks: []event.TaskSummary{
				{ID: "t1", Agent: "architect", Status: "pending"},
				{ID: "t2", Agent: "developer", Status: "pending"},
			},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			TaskAgent:         "architect",
			AgentInputTokens:  1000,
			AgentOutputTokens: 200,
			ResultSummary:     "Designed JWT-based auth with RS256 signing",
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t2"})
		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t2",
			TaskAgent:         "developer",
			AgentInputTokens:  3000,
			AgentOutputTokens: 800,
			ResultSummary:     "Implemented auth endpoints and middleware",
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 5 * time.Second,
		})
	}()

	r.RenderRequest(bus)
	out := buf.String()

	if !strings.Contains(out, "> Designed JWT-based auth with RS256 signing") {
		t.Errorf("expected summary for t1, got: %s", out)
	}
	if !strings.Contains(out, "> Implemented auth endpoints and middleware") {
		t.Errorf("expected summary for t2, got: %s", out)
	}
	// Summary must appear before the completed status line.
	sumIdx := strings.Index(out, "> Designed JWT-based auth")
	statusIdx := strings.Index(out, "OK  t1 architect")
	if sumIdx >= statusIdx {
		t.Errorf("expected summary before status line, summary at %d, status at %d", sumIdx, statusIdx)
	}
}

func TestRenderRequestNoSummaryForEmptyResult(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:   event.TaskComplete,
			TaskID: "t1",
			// No ResultSummary
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 1 * time.Second,
		})
	}()

	r.RenderRequest(bus)
	out := buf.String()

	if strings.Contains(out, "> ") {
		t.Errorf("expected no summary line for empty result, got: %s", out)
	}
}

func TestPrintEventLine(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	r.printEventLine("✓", Green, "t1", "architect", "Designed JWT-based auth", Dim, taskTokens{
		inputTokens: 1234, outputTokens: 567,
	})

	out := buf.String()
	plain := ansiRegex.ReplaceAllString(out, "")

	if !strings.Contains(plain, "✓") {
		t.Error("expected check icon")
	}
	if !strings.Contains(plain, "t1") {
		t.Error("expected task ID")
	}
	if !strings.Contains(plain, "architect") {
		t.Error("expected agent name")
	}
	if !strings.Contains(plain, "──") {
		t.Error("expected separator")
	}
	if !strings.Contains(plain, "Designed JWT-based auth") {
		t.Errorf("expected summary text, got: %s", plain)
	}
	if !strings.Contains(plain, "1,234 in") {
		t.Errorf("expected input tokens, got: %s", plain)
	}
	if !strings.Contains(plain, "567 out") {
		t.Errorf("expected output tokens, got: %s", plain)
	}
}

func TestPrintEventLineNoDetail(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	r.printEventLine("✗", Red, "t2", "developer", "", Red, taskTokens{
		inputTokens: 300, outputTokens: 100,
	})

	out := buf.String()
	plain := ansiRegex.ReplaceAllString(out, "")

	if !strings.Contains(plain, "✗") {
		t.Error("expected failure icon")
	}
	if !strings.Contains(plain, "t2") {
		t.Error("expected task ID")
	}
	if strings.Contains(plain, "──") {
		t.Error("should not have separator when no detail")
	}
	if !strings.Contains(plain, "300 in") {
		t.Errorf("expected tokens even without detail, got: %s", plain)
	}
}

func TestPrintEventLineTruncation(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.width = 50 // narrow terminal

	longSummary := strings.Repeat("x", 200)
	r.printEventLine("✓", Green, "t1", "dev", longSummary, Dim, taskTokens{})

	out := buf.String()
	plain := ansiRegex.ReplaceAllString(out, "")

	if strings.Contains(plain, longSummary) {
		t.Error("expected summary to be truncated for narrow terminal")
	}
	if !strings.Contains(plain, "...") {
		t.Error("expected truncation ellipsis")
	}
}

func TestPrintEventLineTruncationWithTokens(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.width = 50 // narrow terminal where tokens eat all the space

	longSummary := strings.Repeat("x", 200)
	r.printEventLine("✓", Green, "t3", "developer", longSummary, Dim, taskTokens{
		inputTokens: 5282, outputTokens: 10842,
	})

	out := buf.String()
	plain := ansiRegex.ReplaceAllString(out, "")

	// With width=50, prefixLen=21, tokVisLen=25, avail=3 → detail must be omitted.
	if strings.Contains(plain, "xxx") {
		t.Error("expected detail to be omitted when avail < 4")
	}
	// Line must fit in one terminal row (no wrapping).
	visLen := utf8.RuneCountInString(plain) - 1 // -1 for trailing newline
	if visLen > 50 {
		t.Errorf("event line is %d visible chars, exceeds terminal width 50", visLen)
	}
}

func TestPrintEventLineShortSummary(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.width = 120

	shortSummary := "Completed the work."
	r.printEventLine("✓", Green, "t1", "dev", shortSummary, Dim, taskTokens{})

	out := buf.String()
	plain := ansiRegex.ReplaceAllString(out, "")

	if !strings.Contains(plain, shortSummary) {
		t.Error("expected full short summary without truncation")
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		input    string
		n        int
		expected string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "hel"},
		{"hello", 0, ""},
		{"héllo", 3, "hél"},
	}
	for _, tt := range tests {
		got := truncateRunes(tt.input, tt.n)
		if got != tt.expected {
			t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.n, got, tt.expected)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234, "1,234"},
		{12345, "12,345"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
	}
	for _, tt := range tests {
		got := formatTokens(tt.input)
		if got != tt.expected {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := plural("call", 1); got != "call" {
		t.Errorf("plural(call, 1) = %q, want call", got)
	}
	if got := plural("call", 2); got != "calls" {
		t.Errorf("plural(call, 2) = %q, want calls", got)
	}
	if got := plural("call", 0); got != "calls" {
		t.Errorf("plural(call, 0) = %q, want calls", got)
	}
}

func TestTailSnippet(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncated", "hello world", 8, "...world"},
		{"newlines replaced", "line1\nline2\nline3", 50, "line1 line2 line3"},
		{"truncated with newlines", "line1\nline2\nline3", 10, "...2 line3"},
		{"empty", "", 10, ""},
		{"zero maxLen", "hello", 0, ""},
		{"maxLen 3", "abcdef", 3, "def"},
		{"maxLen 4", "abcdef", 4, "...f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tailSnippet(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("tailSnippet(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestVisibleLength(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"plain", "hello", 5},
		{"with ANSI", "\033[32mhello\033[0m", 5},
		{"mixed", "  \033[1m✓\033[0m task1", 9},
		{"no ANSI", "plain text", 10},
		{"empty", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := visibleLength(tt.input)
			if got != tt.want {
				t.Errorf("visibleLength(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestHasRunningTasks(t *testing.T) {
	tests := []struct {
		name  string
		tasks []event.TaskSummary
		want  bool
	}{
		{"none", nil, false},
		{"all pending", []event.TaskSummary{{Status: "pending"}}, false},
		{"one running", []event.TaskSummary{{Status: "pending"}, {Status: "running"}}, true},
		{"all completed", []event.TaskSummary{{Status: "completed"}, {Status: "completed"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasRunningTasks(tt.tasks)
			if got != tt.want {
				t.Errorf("hasRunningTasks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentColorAssignment(t *testing.T) {
	r := NewRenderer(&bytes.Buffer{}, true)

	c1 := r.assignColor("architect")
	c2 := r.assignColor("developer")
	c3 := r.assignColor("architect") // same agent again

	if c1 == c2 {
		t.Error("different agents should get different colors")
	}
	if c1 != c3 {
		t.Error("same agent should get same color")
	}

	// Non-TTY returns empty.
	rNonTTY := NewRenderer(&bytes.Buffer{}, false)
	if got := rNonTTY.assignColor("test"); got != "" {
		t.Errorf("non-TTY assignColor should return empty, got %q", got)
	}
}

func TestDrawRule(t *testing.T) {
	t.Run("TTY with label", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, true)
		r.drawRule("Results")
		out := buf.String()
		if !strings.Contains(out, BoxHorizontal) {
			t.Error("expected box-drawing horizontal in TTY rule")
		}
		if !strings.Contains(out, "Results") {
			t.Error("expected label in TTY rule")
		}
	})

	t.Run("TTY no label", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, true)
		r.drawRule("")
		out := buf.String()
		if !strings.Contains(out, BoxHorizontal) {
			t.Error("expected box-drawing horizontal in TTY rule")
		}
	})

	t.Run("non-TTY with label", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, false)
		r.drawRule("Results")
		out := buf.String()
		if !strings.Contains(out, "--- Results ---") {
			t.Errorf("expected dashed label rule, got: %s", out)
		}
	})

	t.Run("non-TTY no label", func(t *testing.T) {
		var buf bytes.Buffer
		r := NewRenderer(&buf, false)
		r.drawRule("")
		out := buf.String()
		if !strings.Contains(out, "----") {
			t.Error("expected dashes in non-TTY rule")
		}
		if strings.Contains(out, "\033[") {
			t.Error("non-TTY rule should not contain ANSI codes")
		}
	})
}

func TestParseResultBlocks(t *testing.T) {
	input := "**t1** (architect):\nDesigned the system.\n\n---\n\n**t2** (developer):\nImplemented the code.\n"
	blocks := parseResultBlocks(input)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].taskID != "t1" || blocks[0].agent != "architect" {
		t.Errorf("block 0: got taskID=%q agent=%q", blocks[0].taskID, blocks[0].agent)
	}
	if !strings.Contains(blocks[0].text, "Designed the system") {
		t.Errorf("block 0 text: %q", blocks[0].text)
	}
	if blocks[1].taskID != "t2" || blocks[1].agent != "developer" {
		t.Errorf("block 1: got taskID=%q agent=%q", blocks[1].taskID, blocks[1].agent)
	}
	if !strings.Contains(blocks[1].text, "Implemented the code") {
		t.Errorf("block 1 text: %q", blocks[1].text)
	}
}

func TestParseResultBlocksEmpty(t *testing.T) {
	blocks := parseResultBlocks("no structured output here")
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks for unstructured input, got %d", len(blocks))
	}
}

func TestRenderResultsNonTTY(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	input := "**t1** (architect):\nDesigned the system.\n\n---\n\n**t2** (developer):\nBuilt the app.\n"
	r.RenderResults(input)
	out := buf.String()

	if !strings.Contains(out, "t1 (architect):") {
		t.Error("expected t1 header in non-TTY output")
	}
	if !strings.Contains(out, "t2 (developer):") {
		t.Error("expected t2 header in non-TTY output")
	}
	if !strings.Contains(out, "Designed the system") {
		t.Error("expected t1 result text")
	}
	if strings.Contains(out, "\033[") {
		t.Error("non-TTY RenderResults should not contain ANSI codes")
	}
}

func TestRenderResultsTTY(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	input := "**t1** (architect):\nDesigned the system.\n"
	r.RenderResults(input)
	out := buf.String()

	if !strings.Contains(out, "Results") {
		t.Error("expected Results section header")
	}
	if !strings.Contains(out, "t1") {
		t.Error("expected task ID in output")
	}
	if !strings.Contains(out, "architect") {
		t.Error("expected agent name in output")
	}
	// Glamour adds ANSI codes around text; strip them for content check.
	plain := ansiRegex.ReplaceAllString(out, "")
	if !strings.Contains(plain, "Designed the system") {
		t.Errorf("expected result text in output, got: %s", out)
	}
	// Body region (after separator line) should contain ANSI codes from glamour rendering.
	sepIdx := strings.Index(out, strings.Repeat(BoxHorizontal, 16))
	if sepIdx >= 0 {
		bodyRegion := out[sepIdx:]
		if !strings.Contains(bodyRegion, "\033[") {
			t.Error("expected ANSI codes in body region from glamour rendering")
		}
	}
}

func TestRenderResultsUnstructured(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.RenderResults("plain text result")
	out := buf.String()
	if !strings.Contains(out, "plain text result") {
		t.Error("expected unstructured text to pass through")
	}
}

func TestRenderSeparator(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)
	r.RenderSeparator()
	out := buf.String()
	if !strings.Contains(out, "----") {
		t.Error("expected dashes in separator")
	}
}

func TestRenderMarkdown(t *testing.T) {
	r := NewRenderer(&bytes.Buffer{}, true)
	out := r.renderMarkdown("# Hello")
	if !strings.Contains(out, "Hello") {
		t.Error("expected heading text preserved")
	}
	if !strings.Contains(out, "\033[") {
		t.Error("expected ANSI codes in rendered markdown")
	}
}

func TestRenderMarkdownFallback(t *testing.T) {
	r := NewRenderer(&bytes.Buffer{}, true)
	r.glamour = nil // force fallback
	out := r.renderMarkdown("# Hello")
	if out != "# Hello" {
		t.Errorf("expected plain text fallback, got %q", out)
	}
}

func TestRenderRequestScreened(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{
			Type:          event.RequestScreened,
			ScreenMessage: "Hey there! What can I help you build?",
			InputTokens:   50,
			OutputTokens:  30,
			TotalCalls:    1,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 200 * time.Millisecond,
			InputTokens:   50,
			OutputTokens:  30,
			TotalCalls:    1,
		})
	}()

	r.RenderRequest(bus)
	out := buf.String()

	if !strings.Contains(out, "Hey there! What can I help you build?") {
		t.Errorf("expected screen message in output, got: %s", out)
	}
	// RenderRequest defers summary to RenderSummary; token line and "Done" are not printed here.
	if strings.Contains(out, "Done") {
		t.Errorf("screened request should not show full summary in RenderRequest, got: %s", out)
	}
}

func TestRenderRequestScreenedTTY(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{
			Type:          event.RequestScreened,
			ScreenMessage: "Hi! Need help with something?",
			InputTokens:   40,
			OutputTokens:  20,
			TotalCalls:    1,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 150 * time.Millisecond,
			InputTokens:   40,
			OutputTokens:  20,
			TotalCalls:    1,
		})
	}()

	r.RenderRequest(bus)
	out := buf.String()

	if !strings.Contains(out, "Hi! Need help with something?") {
		t.Errorf("expected screen message in TTY output, got: %s", out)
	}
	// TTY output should have Dim styling around the message.
	if !strings.Contains(out, Dim) {
		t.Errorf("expected Dim styling in TTY output, got: %s", out)
	}
	// Should NOT contain full summary.
	if strings.Contains(out, "Done") {
		t.Errorf("screened request should not show full summary in TTY, got: %s", out)
	}
}

func TestRenderRequestPauseResume(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false) // non-TTY for predictable output

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})

		// Trigger pause/resume while running.
		r.Pause()
		// Simulate chat interaction during pause.
		r.Resume()

		bus.Emit(event.Event{
			Type:              event.TaskComplete,
			TaskID:            "t1",
			AgentInputTokens:  100,
			AgentOutputTokens: 50,
		})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 1 * time.Second,
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if !strings.Contains(out, "Done") {
		t.Error("expected Done after pause/resume cycle")
	}
}

func TestRendererAccessors(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)

	if r.TTY() != true {
		t.Error("expected TTY() to return true")
	}
	if r.Writer() != &buf {
		t.Error("expected Writer() to return the buffer")
	}
	if r.Glamour() == nil {
		t.Error("expected Glamour() to return non-nil for TTY renderer")
	}

	r2 := NewRenderer(&buf, false)
	if r2.TTY() != false {
		t.Error("expected TTY() to return false for non-TTY")
	}
	if r2.Glamour() != nil {
		t.Error("expected Glamour() to return nil for non-TTY")
	}
}

func TestRenderMarkdownNonTTY(t *testing.T) {
	r := NewRenderer(&bytes.Buffer{}, false)
	out := r.renderMarkdown("**bold text**")
	if out != "**bold text**" {
		t.Errorf("non-TTY renderMarkdown should return text unchanged, got %q", out)
	}
}

func TestWittyStatus(t *testing.T) {
	// Frame 0 should return the first message.
	got := WittyStatus(0)
	if got != "analyzing task graph..." {
		t.Errorf("WittyStatus(0) = %q, want %q", got, "analyzing task graph...")
	}

	// After 20 frames it should rotate to the next message.
	got2 := WittyStatus(20)
	if got2 == got {
		t.Error("WittyStatus should rotate after 20 frames")
	}

	// Should cycle back after going through all messages.
	total := len(wittyMessages)
	got3 := WittyStatus(total * 20)
	if got3 != got {
		t.Errorf("WittyStatus should cycle, got %q, want %q", got3, got)
	}
}

func TestAnimatedStatusLabel(t *testing.T) {
	// Running status should use witty messages.
	got := animatedStatusLabel("running", 0)
	if got != "analyzing task graph..." {
		t.Errorf("animatedStatusLabel(running, 0) = %q, want %q", got, "analyzing task graph...")
	}

	// Non-running status should use standard labels.
	if got := animatedStatusLabel("completed", 0); got != "completed" {
		t.Errorf("animatedStatusLabel(completed, 0) = %q, want %q", got, "completed")
	}
	if got := animatedStatusLabel("pending", 0); got != "waiting" {
		t.Errorf("animatedStatusLabel(pending, 0) = %q, want %q", got, "waiting")
	}
}

func TestCollapseBlankLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no blanks", "a\nb\nc", "a\nb\nc"},
		{"two blanks ok", "a\n\n\nb", "a\n\n\nb"},
		{"three blanks collapsed", "a\n\n\n\nb", "a\n\n\nb"},
		{"many blanks collapsed", "a\n\n\n\n\n\nb", "a\n\n\nb"},
		{"multiple runs", "a\n\n\n\nb\n\n\n\nc", "a\n\n\nb\n\n\nc"},
		{"empty input", "", ""},
		{"only blanks", "\n\n\n\n", "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collapseBlankLines(tt.input)
			if got != tt.want {
				t.Errorf("collapseBlankLines(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRenderRequestPerModelUsage(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false) // non-TTY for predictable output

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{Type: event.TaskComplete, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 1 * time.Second,
			InputTokens:   5000,
			OutputTokens:  1000,
			TotalCalls:    3,
			ModelUsages: []event.ModelUsage{
				{Model: "anthropic/claude-haiku-4-5", InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200, TotalCalls: 1},
				{Model: "anthropic/claude-sonnet-4", InputTokens: 4000, OutputTokens: 800, TotalTokens: 4800, TotalCalls: 2},
			},
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if !strings.Contains(out, "Usage by model:") {
		t.Errorf("expected per-model usage header, got: %s", out)
	}
	if !strings.Contains(out, "anthropic/claude-haiku-4-5") {
		t.Errorf("expected haiku model in per-model breakdown, got: %s", out)
	}
	if !strings.Contains(out, "anthropic/claude-sonnet-4") {
		t.Errorf("expected sonnet model in per-model breakdown, got: %s", out)
	}
}

func TestRenderRequestSingleModelNoBreakdown(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	bus := event.NewBus()
	go func() {
		bus.Emit(event.Event{Type: event.DecomposeStart})
		bus.Emit(event.Event{
			Type:  event.DecomposeEnd,
			Tasks: []event.TaskSummary{{ID: "t1", Agent: "dev", Status: "pending"}},
		})
		bus.Emit(event.Event{Type: event.TaskStart, TaskID: "t1"})
		bus.Emit(event.Event{Type: event.TaskComplete, TaskID: "t1"})
		bus.Emit(event.Event{
			Type:          event.RequestComplete,
			TotalDuration: 1 * time.Second,
			InputTokens:   5000,
			OutputTokens:  1000,
			TotalCalls:    3,
			ModelUsages: []event.ModelUsage{
				{Model: "anthropic/claude-sonnet-4", InputTokens: 5000, OutputTokens: 1000, TotalTokens: 6000, TotalCalls: 3},
			},
		})
	}()

	r.RenderRequest(bus)
	r.RenderSummary()
	out := buf.String()

	if strings.Contains(out, "Usage by model:") {
		t.Errorf("expected no per-model breakdown with single model, got: %s", out)
	}
}

func TestIndentBlock(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		prefix string
		want   string
	}{
		{"single line", "hello", "  ", "  hello"},
		{"multi line", "line1\nline2\nline3", "  ", "  line1\n  line2\n  line3"},
		{"empty lines indented", "a\n\nb", "  ", "  a\n  \n  b"},
		{"empty input", "", "  ", "  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := indentBlock(tt.text, tt.prefix)
			if got != tt.want {
				t.Errorf("indentBlock(%q, %q) = %q, want %q", tt.text, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestPrintClippedBlockPreservesPinnedTop(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, false)

	lines := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		lines = append(lines, fmt.Sprintf("L%02d", i))
	}

	drawn := r.printClippedBlock(lines, 3)
	out := strings.Split(strings.TrimSpace(buf.String()), "\n")

	if drawn != len(out) {
		t.Fatalf("drawn line count mismatch: got %d, want %d", drawn, len(out))
	}
	if len(out) < 6 {
		t.Fatalf("expected clipped output, got %d lines", len(out))
	}
	if out[0] != "L00" || out[1] != "L01" || out[2] != "L02" {
		t.Fatalf("expected pinned top lines to be preserved, got first lines: %q, %q, %q", out[0], out[1], out[2])
	}
	if !strings.Contains(buf.String(), "... tasks truncated to fit terminal ...") {
		t.Fatal("expected truncation marker for pinned clipping")
	}
	if out[len(out)-1] != "L39" {
		t.Fatalf("expected latest line to remain visible, got %q", out[len(out)-1])
	}
}

func TestPrintClippedBlockTruncatesWideLines(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(&buf, true)
	r.width = 10

	drawn := r.printClippedBlock([]string{
		"12345678901",
		Cyan + "abcdef" + Reset,
	}, 0)

	// Lines wider than terminal are truncated to prevent soft wrapping,
	// so each line takes exactly 1 physical row.
	if drawn != 2 {
		t.Fatalf("expected 2 physical rows, got %d", drawn)
	}
	// The first line should have been truncated to 9 visible chars.
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if vl := visibleLength(lines[0]); vl > 9 {
		t.Fatalf("expected truncated line ≤9 visible chars, got %d", vl)
	}
}

func TestEraseBlockReturnsToColumnZero(t *testing.T) {
	var buf bytes.Buffer
	EraseBlock(&buf, 4)

	if got := buf.String(); !strings.HasPrefix(got, "\r\033[4A") {
		t.Fatalf("EraseBlock should carriage-return before moving up, got %q", got)
	}
}
