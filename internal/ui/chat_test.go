package ui

import (
	"bytes"
	"strings"
	"testing"
)

// chanReadLine returns a readLine function backed by a channel, for testing.
func chanReadLine(ch <-chan string) func() (string, bool) {
	return func() (string, bool) {
		s, ok := <-ch
		return s, ok
	}
}

func TestPickAgentSelection(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
		{Name: "architect", Status: "running", TaskID: "t1"},
		{Name: "developer", Status: "idle"},
	}

	ch := make(chan string, 1)
	ch <- "2"

	got := PickAgent(&buf, agents, chanReadLine(ch), false, nil)
	if got != "architect" {
		t.Errorf("expected architect, got %q", got)
	}
}

func TestPickAgentCancel(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
	}

	ch := make(chan string, 1)
	ch <- ""

	got := PickAgent(&buf, agents, chanReadLine(ch), false, nil)
	if got != "" {
		t.Errorf("expected empty for cancel, got %q", got)
	}
}

func TestPickAgentInvalidNumber(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
	}

	ch := make(chan string, 1)
	ch <- "5"

	got := PickAgent(&buf, agents, chanReadLine(ch), false, nil)
	if got != "" {
		t.Errorf("expected empty for invalid number, got %q", got)
	}
}

func TestPickAgentOrdering(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
		{Name: "architect", Status: "running", TaskID: "t1"},
		{Name: "developer", Status: "idle"},
	}

	ch := make(chan string, 1)
	ch <- "1"

	got := PickAgent(&buf, agents, chanReadLine(ch), false, nil)
	if got != "conductor" {
		t.Errorf("first agent should be conductor, got %q", got)
	}
}

func TestPickAgentTTYFallback(t *testing.T) {
	// TTY but no TermInput → falls back to numbered list.
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
		{Name: "developer", Status: "running", TaskID: "t1"},
		{Name: "designer", Status: "idle"},
	}

	ch := make(chan string, 1)
	ch <- "1"

	PickAgent(&buf, agents, chanReadLine(ch), true, nil)
	out := buf.String()

	if !strings.Contains(out, "conductor") {
		t.Error("expected conductor in TTY fallback output")
	}
	if !strings.Contains(out, "●") {
		t.Error("expected filled dot for conductor/running agents")
	}
	if !strings.Contains(out, "·") {
		t.Error("expected open dot for idle agents")
	}
}

func TestPickAgentNonTTYOutput(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "conductor", Status: "conductor"},
		{Name: "developer", Status: "running", TaskID: "t1"},
	}

	ch := make(chan string, 1)
	ch <- "1"

	PickAgent(&buf, agents, chanReadLine(ch), false, nil)
	out := buf.String()

	if !strings.Contains(out, "[1] conductor (conductor)") {
		t.Errorf("expected formatted conductor entry, got: %s", out)
	}
	if !strings.Contains(out, "(running t1)") {
		t.Errorf("expected running status, got: %s", out)
	}
	if strings.Contains(out, "\033[") {
		t.Error("non-TTY output should not contain ANSI codes")
	}
}

func TestRenderChatResponseTTY(t *testing.T) {
	var buf bytes.Buffer
	RenderChatResponse(&buf, "architect", "Here is the answer.", true, nil)
	out := buf.String()

	if !strings.Contains(out, "architect") {
		t.Error("expected agent name")
	}
	if !strings.Contains(out, "Here is the answer.") {
		t.Error("expected response text")
	}
}

func TestRenderChatResponseNonTTY(t *testing.T) {
	var buf bytes.Buffer
	RenderChatResponse(&buf, "architect", "Here is the answer.", false, nil)
	out := buf.String()

	if out != "architect: Here is the answer.\n" {
		t.Errorf("unexpected output: %q", out)
	}
	if strings.Contains(out, "\033[") {
		t.Error("non-TTY output should not contain ANSI codes")
	}
}

func TestRenderAgentQueryTTY(t *testing.T) {
	var buf bytes.Buffer
	RenderAgentQuery(&buf, "developer", "What auth provider?", true)
	out := buf.String()

	if !strings.Contains(out, "?") {
		t.Error("expected question mark icon")
	}
	if !strings.Contains(out, "developer") {
		t.Error("expected agent name")
	}
	if !strings.Contains(out, "What auth provider?") {
		t.Error("expected question text")
	}
}

func TestRenderAgentQueryNonTTY(t *testing.T) {
	var buf bytes.Buffer
	RenderAgentQuery(&buf, "developer", "What auth provider?", false)
	out := buf.String()

	if !strings.Contains(out, "developer asks: What auth provider?") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestPickAgentEmpty(t *testing.T) {
	var buf bytes.Buffer
	got := PickAgent(&buf, nil, nil, false, nil)
	if got != "" {
		t.Errorf("expected empty for nil agents, got %q", got)
	}
}

func TestPromptReplyWithOptionsNonTTY(t *testing.T) {
	t.Run("select by number", func(t *testing.T) {
		var buf bytes.Buffer
		ch := make(chan string, 1)
		ch <- "2"

		options := []string{"Yes, proceed", "No, change approach", "Tell me more"}
		got := PromptReplyWithOptions(&buf, options, chanReadLine(ch), false, nil)
		if got != "No, change approach" {
			t.Errorf("expected option 2, got %q", got)
		}
		out := buf.String()
		if !strings.Contains(out, "[1] Yes, proceed") {
			t.Errorf("expected numbered options, got: %s", out)
		}
	})

	t.Run("free text fallback", func(t *testing.T) {
		var buf bytes.Buffer
		ch := make(chan string, 1)
		ch <- "Something custom"

		options := []string{"Yes", "No"}
		got := PromptReplyWithOptions(&buf, options, chanReadLine(ch), false, nil)
		if got != "Something custom" {
			t.Errorf("expected free text, got %q", got)
		}
	})

	t.Run("no options falls through to PromptReply", func(t *testing.T) {
		var buf bytes.Buffer
		ch := make(chan string, 1)
		ch <- "hello"

		got := PromptReplyWithOptions(&buf, nil, chanReadLine(ch), false, nil)
		if got != "hello" {
			t.Errorf("expected hello, got %q", got)
		}
	})

	t.Run("invalid number returns as text", func(t *testing.T) {
		var buf bytes.Buffer
		ch := make(chan string, 1)
		ch <- "99"

		options := []string{"Yes", "No"}
		got := PromptReplyWithOptions(&buf, options, chanReadLine(ch), false, nil)
		if got != "99" {
			t.Errorf("expected '99' as free text, got %q", got)
		}
	})
}

func TestPromptReplyNonTTY(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan string, 1)
	ch <- "Sure, go ahead"

	got := PromptReply(&buf, chanReadLine(ch), false, nil)
	if got != "Sure, go ahead" {
		t.Errorf("expected typed reply, got %q", got)
	}
	if !strings.Contains(buf.String(), "> ") {
		t.Error("expected > prompt for non-TTY")
	}
}

func TestPromptReplyNonTTYEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan string, 1)
	ch <- ""

	got := PromptReply(&buf, chanReadLine(ch), false, nil)
	if got != "" {
		t.Errorf("expected empty for blank reply, got %q", got)
	}
}

func TestDrawAgentPicker(t *testing.T) {
	var buf bytes.Buffer
	agents := []AgentInfo{
		{Name: "architect", Status: "running", TaskID: "t1"},
		{Name: "developer", Status: "idle"},
	}

	lines := drawAgentPicker(&buf, agents, 0)
	out := buf.String()

	// Should return agent count + 1 hint line.
	if lines != 3 {
		t.Errorf("expected 3 lines, got %d", lines)
	}
	// Selected agent should have the arrow indicator.
	if !strings.Contains(out, "▸") {
		t.Error("expected selection arrow for first agent")
	}
	if !strings.Contains(out, "architect") {
		t.Error("expected architect in output")
	}
	if !strings.Contains(out, "navigate") {
		t.Error("expected hint line with navigate")
	}
}

func TestEraseLines(t *testing.T) {
	var buf bytes.Buffer
	eraseLines(&buf, 3)
	out := buf.String()

	expected := MoveUp + ClearLn + MoveUp + ClearLn + MoveUp + ClearLn
	if out != expected {
		t.Errorf("expected 3x MoveUp+ClearLn, got %q", out)
	}
}
