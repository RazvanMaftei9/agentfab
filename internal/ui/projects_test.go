package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPickProjectText_SelectExisting(t *testing.T) {
	projects := []ProjectInfo{
		{Name: "my-app", Dir: "/home/user/Documents/my-app", LastUsedAt: time.Now()},
		{Name: "backend", Dir: "/home/user/Documents/backend", LastUsedAt: time.Now().Add(-24 * time.Hour)},
	}

	var buf bytes.Buffer
	calls := 0
	readLine := func() (string, bool) {
		calls++
		return "1", true
	}

	result := PickProject(&buf, projects, readLine, false, nil)
	if result != "my-app" {
		t.Errorf("PickProject = %q, want %q", result, "my-app")
	}
}

func TestPickProjectText_SelectNew(t *testing.T) {
	projects := []ProjectInfo{
		{Name: "my-app", Dir: "/home/user/my-app", LastUsedAt: time.Now()},
	}

	var buf bytes.Buffer
	readLine := func() (string, bool) {
		return "n", true
	}

	result := PickProject(&buf, projects, readLine, false, nil)
	if result != "new" {
		t.Errorf("PickProject = %q, want %q", result, "new")
	}
}

func TestPickProjectText_Cancel(t *testing.T) {
	projects := []ProjectInfo{
		{Name: "my-app", Dir: "/home/user/my-app", LastUsedAt: time.Now()},
	}

	var buf bytes.Buffer
	readLine := func() (string, bool) {
		return "", true
	}

	result := PickProject(&buf, projects, readLine, false, nil)
	if result != "" {
		t.Errorf("PickProject = %q, want empty", result)
	}
}

func TestPickProjectText_InvalidInput(t *testing.T) {
	projects := []ProjectInfo{
		{Name: "my-app", Dir: "/home/user/my-app", LastUsedAt: time.Now()},
	}

	var buf bytes.Buffer
	readLine := func() (string, bool) {
		return "999", true
	}

	result := PickProject(&buf, projects, readLine, false, nil)
	if result != "" {
		t.Errorf("PickProject = %q, want empty for invalid input", result)
	}
}

func TestPickProjectText_EmptyList(t *testing.T) {
	var buf bytes.Buffer
	readLine := func() (string, bool) {
		return "n", true
	}

	result := PickProject(&buf, nil, readLine, false, nil)
	if result != "new" {
		t.Errorf("PickProject with empty list = %q, want %q", result, "new")
	}
}

func TestPickProjectText_NonTTYOutput(t *testing.T) {
	projects := []ProjectInfo{
		{Name: "my-app", Dir: "/home/user/my-app", LastUsedAt: time.Now()},
	}

	var buf bytes.Buffer
	readLine := func() (string, bool) {
		return "1", true
	}

	PickProject(&buf, projects, readLine, false, nil)
	output := buf.String()

	if !strings.Contains(output, "Select a project:") {
		t.Error("expected 'Select a project:' header in non-TTY output")
	}
	if !strings.Contains(output, "[1] my-app") {
		t.Error("expected numbered project listing")
	}
	if !strings.Contains(output, "[n] Create new project") {
		t.Error("expected 'Create new project' option")
	}
}

func TestRelativeTime(t *testing.T) {
	tests := []struct {
		dur  time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{5 * time.Minute, "5 minutes ago"},
		{1 * time.Minute, "1 minute ago"},
		{3 * time.Hour, "3 hours ago"},
		{1 * time.Hour, "1 hour ago"},
		{48 * time.Hour, "2 days ago"},
		{24 * time.Hour, "1 day ago"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := relativeTime(time.Now().Add(-tt.dur))
			if got != tt.want {
				t.Errorf("relativeTime(-%v) = %q, want %q", tt.dur, got, tt.want)
			}
		})
	}

	if got := relativeTime(time.Time{}); got != "never" {
		t.Errorf("relativeTime(zero) = %q, want %q", got, "never")
	}
}
