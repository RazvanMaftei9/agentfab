package loop

import (
	"strings"
	"testing"
)

func TestResolveNextState(t *testing.T) {
	def := reviewLoop()

	tests := []struct {
		name    string
		state   string
		verdict string
		want    string
		wantErr bool
	}{
		{"working_no_condition", "WORKING", "", "REVIEWING", false},
		{"reviewing_approved", "REVIEWING", "approved", "APPROVED", false},
		{"reviewing_revise", "REVIEWING", "revise", "REVISING", false},
		{"reviewing_no_verdict", "REVIEWING", "", "", true},
		{"revising_no_condition", "REVISING", "", "REVIEWING", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveNextState(&def, tc.state, tc.verdict)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsReviewState(t *testing.T) {
	def := reviewLoop()

	if IsReviewState(&def, "WORKING") {
		t.Error("WORKING should not be a review state")
	}
	if !IsReviewState(&def, "REVIEWING") {
		t.Error("REVIEWING should be a review state")
	}
	if IsReviewState(&def, "REVISING") {
		t.Error("REVISING should not be a review state")
	}
	if IsReviewState(&def, "APPROVED") {
		t.Error("APPROVED should not be a review state")
	}
}

func TestBuildStatePrompt(t *testing.T) {
	def := reviewLoop()

	t.Run("initial_state", func(t *testing.T) {
		got := BuildStatePrompt("Implement login", &def, "WORKING", "", "", "", nil)
		if !strings.Contains(got, "Implement login") {
			t.Error("missing task description")
		}
		if !strings.Contains(got, "initial work phase") {
			t.Error("missing initial state guidance")
		}
	})

	t.Run("review_state", func(t *testing.T) {
		got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", "", nil)
		if !strings.Contains(got, "VERDICT:") {
			t.Error("missing verdict instruction")
		}
		if !strings.Contains(got, "do not invent requirements") {
			t.Error("missing anti-invention guardrail for reviews")
		}
	})

	t.Run("revision_after_revise", func(t *testing.T) {
		got := BuildStatePrompt("Implement login", &def, "REVISING", "revise", "fix errors", "", nil)
		if !strings.Contains(got, "needs revision") {
			t.Error("missing revision guidance")
		}
		if !strings.Contains(got, "$SCRATCH_DIR") {
			t.Error("missing scratch context for REVISING state")
		}
		if !strings.Contains(got, "do not regenerate") {
			t.Error("missing anti-regeneration guidance for REVISING state")
		}
		if !strings.Contains(got, "```file:path```") {
			t.Error("missing file block guidance for REVISING state")
		}
		if !strings.Contains(got, "source-of-truth constraints") {
			t.Error("missing source-of-truth guardrail for revisions")
		}
		if !strings.Contains(got, "conflicts with upstream artifacts") {
			t.Error("missing conflict-resolution guardrail for revisions")
		}
	})
}

func TestBuildStatePromptReviewComparison(t *testing.T) {
	def := reviewLoop()
	got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", "", nil)
	if !strings.Contains(got, "broadly correct") {
		t.Error("missing broad correctness instruction in review prompt")
	}
	if !strings.Contains(got, "Required features") {
		t.Error("missing feature check instruction")
	}
	if !strings.Contains(got, "Layout structure") {
		t.Error("missing layout check instruction")
	}
	if !strings.Contains(got, "No broken functionality") {
		t.Error("missing broken functionality check instruction")
	}
	if !strings.Contains(got, "Do NOT flag minor cosmetic") {
		t.Error("missing cosmetic leniency instruction")
	}
}

func TestBuildStatePromptRevisionRecheck(t *testing.T) {
	def := reviewLoop()
	got := BuildStatePrompt("Implement login", &def, "REVISING", "revise", "fix errors", "", nil)
	if !strings.Contains(got, "Upstream dependency artifacts have not changed") {
		t.Error("missing upstream artifact stability note in revision prompt")
	}
	if !strings.Contains(got, "Only re-read them if the reviewer questions") {
		t.Error("missing conditional re-read guidance")
	}
}

func TestBuildStatePromptRevisionRereadEnforcement(t *testing.T) {
	def := reviewLoop()
	got := BuildStatePrompt("Implement login", &def, "REVISING", "revise", "fix errors", "", nil)
	if !strings.Contains(got, "Re-read the flagged implementation files") {
		t.Error("missing implementation file re-read instruction in revision prompt")
	}
	if !strings.Contains(got, "no changes needed") {
		t.Error("missing anti-stonewalling instruction in revision prompt")
	}
}

func TestBuildStatePromptRevisionWithFileManifest(t *testing.T) {
	def := reviewLoop()
	files := []string{"src/App.tsx", "src/index.css", "package.json"}
	got := BuildStatePrompt("Implement login", &def, "REVISING", "revise", "fix errors", "", files)
	if !strings.Contains(got, "Files you produced in the previous iteration") {
		t.Error("missing file manifest header in revision prompt")
	}
	if !strings.Contains(got, "src/App.tsx") {
		t.Error("missing file entry in manifest")
	}
	if !strings.Contains(got, "Do not waste a tool call listing these") {
		t.Error("missing tool-call saving instruction")
	}
}

func TestBuildStatePromptCustomGuidelines(t *testing.T) {
	def := reviewLoop()
	// Set custom review guidelines on the REVIEWING state.
	for i, s := range def.States {
		if s.Name == "REVIEWING" {
			def.States[i].ReviewGuidelines = "- Check color codes match spec\n- Verify font families"
			break
		}
	}

	got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", "", nil)
	if !strings.Contains(got, "Focus your review on these areas") {
		t.Error("missing custom guidelines header")
	}
	if !strings.Contains(got, "Check color codes match spec") {
		t.Error("missing custom guideline content")
	}
	// Fallback content should NOT appear when custom guidelines are set.
	if strings.Contains(got, "broadly correct") {
		t.Error("fallback checklist should not appear when custom guidelines are set")
	}
}

func TestBuildStatePromptFallbackWithoutGuidelines(t *testing.T) {
	def := reviewLoop()
	// No ReviewGuidelines set — should use the fallback checklist.
	got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", "", nil)
	if !strings.Contains(got, "broadly correct") {
		t.Error("fallback checklist should appear when no custom guidelines are set")
	}
	if strings.Contains(got, "Focus your review on these areas") {
		t.Error("custom guidelines header should not appear without guidelines")
	}
}

func TestBuildStatePromptDecisionContextInReview(t *testing.T) {
	def := reviewLoop()
	decisions := "Active project decisions:\n- [architect] Use Material Design 3 for all UI components"

	got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", decisions, nil)
	if !strings.Contains(got, "Material Design 3") {
		t.Error("decision context should appear in review prompt")
	}
	if !strings.Contains(got, "adheres to these active project decisions") {
		t.Error("missing decision enforcement instruction in review prompt")
	}
}

func TestBuildStatePromptDecisionContextNotInWorkState(t *testing.T) {
	def := reviewLoop()
	decisions := "Active project decisions:\n- [architect] Use Material Design 3"

	// Decision context should NOT appear in the initial work state.
	got := BuildStatePrompt("Implement login", &def, "WORKING", "", "", decisions, nil)
	if strings.Contains(got, "Material Design 3") {
		t.Error("decision context should not appear in initial work state")
	}
}

func TestBuildStatePromptEmptyDecisionContext(t *testing.T) {
	def := reviewLoop()
	// Empty decision context should not add any decision-related text.
	got := BuildStatePrompt("Implement login", &def, "REVIEWING", "", "some code", "", nil)
	if strings.Contains(got, "adheres to these active project decisions") {
		t.Error("empty decision context should not produce enforcement text")
	}
}

func TestReviewGuidelinesForState(t *testing.T) {
	def := reviewLoop()
	// Initially no guidelines.
	if g := def.ReviewGuidelinesForState("REVIEWING"); g != "" {
		t.Errorf("expected empty guidelines, got %q", g)
	}

	// Set guidelines.
	for i, s := range def.States {
		if s.Name == "REVIEWING" {
			def.States[i].ReviewGuidelines = "check imports"
			break
		}
	}
	if g := def.ReviewGuidelinesForState("REVIEWING"); g != "check imports" {
		t.Errorf("expected 'check imports', got %q", g)
	}

	// Non-existent state returns empty.
	if g := def.ReviewGuidelinesForState("NONEXISTENT"); g != "" {
		t.Errorf("expected empty for non-existent state, got %q", g)
	}
}
