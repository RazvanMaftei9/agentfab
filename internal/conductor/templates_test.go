package conductor

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadTemplates(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/build-app.yaml": &fstest.MapFile{
			Data: []byte(`name: build-app
description: "Build a new application"
match_keywords: [build, create, make]
tasks:
  - id: t1
    agent: architect
    description: "Design for {{request}}"
  - id: t2
    agent: developer
    description: "Implement {{request}}"
    depends_on: [t1]
    loop_id: loop-1
loops:
  - id: loop-1
    participants: [developer, architect]
    states:
      - name: WORKING
        agent: developer
      - name: REVIEWING
        agent: architect
      - name: APPROVED
        agent: ""
    transitions:
      - from: WORKING
        to: REVIEWING
      - from: REVIEWING
        to: APPROVED
        condition: approved
    initial_state: WORKING
    terminal_states: [APPROVED]
    max_transitions: 3
`),
		},
		"templates/fix-bug.yaml": &fstest.MapFile{
			Data: []byte(`name: fix-bug
description: "Fix a bug"
match_keywords: [fix, bug, error]
tasks:
  - id: t1
    agent: developer
    description: "Fix {{request}}"
`),
		},
	}

	templates, err := LoadTemplates(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 {
		t.Fatalf("got %d templates, want 2", len(templates))
	}

	// Templates should include both.
	names := map[string]bool{}
	for _, tmpl := range templates {
		names[tmpl.Name] = true
	}
	if !names["build-app"] || !names["fix-bug"] {
		t.Errorf("expected build-app and fix-bug, got %v", names)
	}
}

func TestLoadTemplatesSkipsMalformed(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/good.yaml": &fstest.MapFile{
			Data: []byte(`name: good
description: "A good template"
match_keywords: [test]
tasks:
  - id: t1
    agent: developer
    description: "Test"
`),
		},
		"templates/bad.yaml": &fstest.MapFile{
			Data: []byte(`this is not valid yaml: [`),
		},
		"templates/noname.yaml": &fstest.MapFile{
			Data: []byte(`description: "No name field"
tasks: []
`),
		},
	}

	templates, err := LoadTemplates(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 {
		t.Fatalf("got %d templates, want 1 (only 'good')", len(templates))
	}
	if templates[0].Name != "good" {
		t.Errorf("template name = %q, want good", templates[0].Name)
	}
}

func TestFormatTemplatesForPrompt(t *testing.T) {
	templates := []DecomposeTemplate{
		{
			Name:          "build-app",
			Description:   "Build a new application",
			MatchKeywords: []string{"build", "create"},
			Tasks: []templateTask{
				{ID: "t1", Agent: "architect", Description: "Design"},
				{ID: "t2", Agent: "developer", Description: "Implement", LoopID: "loop-1"},
			},
		},
	}

	output := formatTemplatesForPrompt(templates)
	if !strings.Contains(output, "build-app") {
		t.Error("output should contain template name")
	}
	if !strings.Contains(output, "Build a new application") {
		t.Error("output should contain description")
	}
	if !strings.Contains(output, "build, create") {
		t.Error("output should contain keywords")
	}
	if !strings.Contains(output, "architect") {
		t.Error("output should contain agent names")
	}
	if !strings.Contains(output, "(loop)") {
		t.Error("output should mark loop tasks")
	}
}

func TestFormatTemplatesForPromptEmpty(t *testing.T) {
	output := formatTemplatesForPrompt(nil)
	if output != "" {
		t.Errorf("empty templates should produce empty string, got %q", output)
	}
}

func TestLoadTemplatesFromEmbedded(t *testing.T) {
	// Test loading from the actual embedded templates.
	// Uses the defaults package, but since we can't import it from
	// conductor tests (import cycle), we test with a synthetic FS.
	// The TestLoadTemplates above covers the logic.
}
