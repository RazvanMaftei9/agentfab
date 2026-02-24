package agent

import "testing"

func TestExtractEnvelope_ValidJSON(t *testing.T) {
	content := `I reviewed the implementation. Here are my findings:

` + "```json\n" + `{
  "type": "verdict",
  "verdict": "revise",
  "summary": "CSS colors don't match spec",
  "issues": [
    {
      "file": "src/styles.css",
      "line": 42,
      "severity": "error",
      "expected": "#1976D2",
      "actual": "#2196F3",
      "message": "Primary color doesn't match spec",
      "source": "artifacts/designer/spec.md"
    }
  ]
}` + "\n```\n" + `
The implementation has color mismatches.`

	env := extractEnvelope(content)
	if env == nil {
		t.Fatal("expected non-nil envelope")
	}
	if env.Type != "verdict" {
		t.Errorf("Type = %q, want verdict", env.Type)
	}
	if env.Verdict != "revise" {
		t.Errorf("Verdict = %q, want revise", env.Verdict)
	}
	if len(env.Issues) != 1 {
		t.Fatalf("Issues count = %d, want 1", len(env.Issues))
	}
	if env.Issues[0].File != "src/styles.css" {
		t.Errorf("Issue file = %q", env.Issues[0].File)
	}
	if env.Issues[0].Expected != "#1976D2" {
		t.Errorf("Issue expected = %q", env.Issues[0].Expected)
	}
}

func TestExtractEnvelope_NoJSON(t *testing.T) {
	content := "VERDICT: approved\n\nLooks good, all checks pass."
	env := extractEnvelope(content)
	if env != nil {
		t.Errorf("expected nil envelope for non-JSON content, got %+v", env)
	}
}

func TestExtractEnvelope_NonEnvelopeJSON(t *testing.T) {
	content := `{"name": "todo-app", "tasks": [{"id": "t1"}]}`
	env := extractEnvelope(content)
	if env != nil {
		t.Errorf("expected nil for non-envelope JSON (no type field), got %+v", env)
	}
}

func TestExtractVerdictFromEnvelope_EnvelopeApproved(t *testing.T) {
	content := "```json\n" + `{"type": "verdict", "verdict": "approved", "summary": "All good"}` + "\n```"
	v := extractVerdictFromEnvelope(content)
	if v != "approved" {
		t.Errorf("verdict = %q, want approved", v)
	}
}

func TestExtractVerdictFromEnvelope_EnvelopeRevise(t *testing.T) {
	content := "```json\n" + `{"type": "verdict", "verdict": "revise", "summary": "Issues found"}` + "\n```"
	v := extractVerdictFromEnvelope(content)
	if v != "revise" {
		t.Errorf("verdict = %q, want revise", v)
	}
}

func TestExtractVerdictFromEnvelope_FallbackToLegacy(t *testing.T) {
	content := "VERDICT: approved\n\nAll implementation matches spec."
	v := extractVerdictFromEnvelope(content)
	if v != "approved" {
		t.Errorf("verdict = %q, want approved (legacy fallback)", v)
	}
}

func TestExtractVerdictFromEnvelope_FallbackHeuristic(t *testing.T) {
	content := "Looks good to me, everything is correct."
	v := extractVerdictFromEnvelope(content)
	if v != "approved" {
		t.Errorf("verdict = %q, want approved (heuristic fallback)", v)
	}
}

func TestExtractEnvelope_TaskResult(t *testing.T) {
	content := "```json\n" + `{
  "type": "task_result",
  "status": "success",
  "summary": "Implemented todo app",
  "artifacts": ["src/index.html", "src/styles.css", "src/app.js"]
}` + "\n```"

	env := extractEnvelope(content)
	if env == nil {
		t.Fatal("expected non-nil envelope")
	}
	if env.Type != "task_result" {
		t.Errorf("Type = %q, want task_result", env.Type)
	}
	if env.Status != "success" {
		t.Errorf("Status = %q, want success", env.Status)
	}
	if len(env.Artifacts) != 3 {
		t.Errorf("Artifacts count = %d, want 3", len(env.Artifacts))
	}
}
