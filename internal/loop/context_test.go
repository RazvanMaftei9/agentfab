package loop

import (
	"strings"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/message"
)

func TestLoopContextRoundTrip(t *testing.T) {
	lc := &LoopContext{
		Definition:   reviewLoop(),
		State:        LoopState{LoopID: "review-loop", CurrentState: "WORKING", TransitionCount: 1},
		TaskID:       "t1",
		Conductor:    "conductor",
		OriginalTask: "Implement login",
		UserRequest:  "Build a login page",
		DepParts: []map[string]any{
			{"dependency_id": "t0", "result": "API spec"},
		},
		WorkSummary:       "Implemented login handler and tests.",
		WorkArtifactURI:   "artifacts/developer/",
		WorkArtifactFiles: []string{"src/login.go", "src/login_test.go"},
	}

	dp := EncodeContext(lc)

	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "some task"},
			dp,
		},
	}

	decoded, ok := DecodeContext(msg)
	if !ok {
		t.Fatal("DecodeContext returned false")
	}

	if decoded.TaskID != "t1" {
		t.Errorf("TaskID: got %q", decoded.TaskID)
	}
	if decoded.Conductor != "conductor" {
		t.Errorf("Conductor: got %q", decoded.Conductor)
	}
	if decoded.OriginalTask != "Implement login" {
		t.Errorf("OriginalTask: got %q", decoded.OriginalTask)
	}
	if decoded.UserRequest != "Build a login page" {
		t.Errorf("UserRequest: got %q", decoded.UserRequest)
	}
	if decoded.State.CurrentState != "WORKING" {
		t.Errorf("State: got %q", decoded.State.CurrentState)
	}
	if decoded.State.TransitionCount != 1 {
		t.Errorf("TransitionCount: got %d", decoded.State.TransitionCount)
	}
	if decoded.Definition.ID != "review-loop" {
		t.Errorf("Definition.ID: got %q", decoded.Definition.ID)
	}
	if len(decoded.DepParts) != 1 {
		t.Fatalf("DepParts: got %d", len(decoded.DepParts))
	}
	if decoded.WorkSummary == "" {
		t.Error("expected WorkSummary to round-trip")
	}
	if decoded.WorkArtifactURI != "artifacts/developer/" {
		t.Errorf("WorkArtifactURI: got %q", decoded.WorkArtifactURI)
	}
	if len(decoded.WorkArtifactFiles) != 2 {
		t.Errorf("WorkArtifactFiles: got %d", len(decoded.WorkArtifactFiles))
	}
}

func TestFilterDepParts(t *testing.T) {
	def := reviewLoop()

	archDep := map[string]any{"dependency_id": "t0", "dependency_agent": "architect", "result": "design.md"}
	designerDep := map[string]any{"dependency_id": "t1", "dependency_agent": "designer", "result": "spec.md"}
	deps := []map[string]any{archDep, designerDep}

	t.Run("review_excludes_self", func(t *testing.T) {
		got := FilterDepParts(deps, &def, "REVIEWING", "", "architect")
		if len(got) != 1 {
			t.Fatalf("expected 1 dep, got %d", len(got))
		}
		if got[0]["dependency_agent"] != "designer" {
			t.Errorf("expected designer dep, got %v", got[0]["dependency_agent"])
		}
	})

	t.Run("revision_keeps_compacted_binding_context", func(t *testing.T) {
		long := strings.Repeat("x", 2000)
		revisionDeps := []map[string]any{
			{
				"dependency_id":    "t0",
				"dependency_agent": "architect",
				"artifact_uri":     "artifacts/architect/",
				"result":           long,
			},
			{
				"dependency_id":    "t1",
				"dependency_agent": "designer",
				"result":           "spec.md",
			},
		}

		got := FilterDepParts(revisionDeps, &def, "REVISING", "revise", "developer")
		if len(got) != 2 {
			t.Fatalf("expected 2 deps, got %d", len(got))
		}
		r0, _ := got[0]["result"].(string)
		if len(r0) >= len(long) {
			t.Fatalf("expected compacted result, got %d chars (original %d)", len(r0), len(long))
		}
		if !strings.Contains(r0, "truncated for revision context") {
			t.Errorf("expected truncation marker in compacted result, got: %q", r0)
		}
	})

	t.Run("revision_preserves_more_loop_feedback_with_artifacts", func(t *testing.T) {
		long := strings.Repeat("r", 2000)
		revisionDeps := []map[string]any{
			{
				"dependency_id":    "t3",
				"dependency_agent": "architect",
				"context_kind":     "loop_feedback",
				"artifact_uri":     "artifacts/developer/",
				"result":           long,
			},
		}

		got := FilterDepParts(revisionDeps, &def, "REVISING", "revise", "developer")
		if len(got) != 1 {
			t.Fatalf("expected 1 dep, got %d", len(got))
		}
		r0, _ := got[0]["result"].(string)
		if len(r0) <= 320 {
			t.Fatalf("expected loop feedback to keep more detail than URI compact mode, got %d chars", len(r0))
		}
		if len(r0) >= len(long) {
			t.Fatalf("expected some compaction, got %d chars (original %d)", len(r0), len(long))
		}
	})

	t.Run("initial_work_passes_through", func(t *testing.T) {
		got := FilterDepParts(deps, &def, "WORKING", "", "developer")
		if len(got) != 2 {
			t.Errorf("expected 2 deps, got %d", len(got))
		}
	})

	t.Run("review_no_self_match", func(t *testing.T) {
		otherDeps := []map[string]any{designerDep}
		got := FilterDepParts(otherDeps, &def, "REVIEWING", "", "architect")
		if len(got) != 1 {
			t.Errorf("expected 1 dep, got %d", len(got))
		}
	})

	t.Run("nil_depparts", func(t *testing.T) {
		got := FilterDepParts(nil, &def, "REVIEWING", "", "architect")
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

func TestDecodeContextNotFound(t *testing.T) {
	msg := &message.Message{
		Parts: []message.Part{
			message.TextPart{Text: "no loop context"},
		},
	}
	_, ok := DecodeContext(msg)
	if ok {
		t.Error("expected false for message without loop_context")
	}
}
