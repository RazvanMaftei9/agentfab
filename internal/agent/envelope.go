package agent

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/structuredoutput"
)

// ResponseEnvelope carries machine-readable metadata in agent responses
// so the system doesn't parse prose. File content stays in file:path blocks.
type ResponseEnvelope struct {
	Type      string        `json:"type"`                // task_result, verdict, escalate, ask_user
	Status    string        `json:"status,omitempty"`    // success, failure, blocked
	Verdict   string        `json:"verdict,omitempty"`   // approved, revise (review states only)
	Summary   string        `json:"summary,omitempty"`   // one-line human description
	Artifacts []string      `json:"artifacts,omitempty"` // list of produced file paths
	Issues    []ReviewIssue `json:"issues,omitempty"`    // structured review feedback
	Questions []string      `json:"questions,omitempty"` // questions for user
}

type ReviewIssue struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"` // error, warning, info
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"` // reference to spec/artifact
}

const EnvelopeJSONSchema = `{
  "type": "task_result | verdict | escalate | ask_user",
  "status": "success | failure | blocked",
  "verdict": "approved | revise",
  "summary": "one-line description of what was done or decided",
  "artifacts": ["path/to/file1.html", "path/to/styles.css"],
  "issues": [
    {
      "file": "src/styles.css",
      "line": 42,
      "severity": "error | warning | info",
      "expected": "expected value",
      "actual": "actual value",
      "message": "description of the issue",
      "source": "reference to spec or artifact"
    }
  ]
}`

func extractEnvelope(content string) *ResponseEnvelope {
	raw, err := structuredoutput.ExtractJSONFromContent(content)
	if err != nil {
		return nil
	}

	var env ResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil
	}

	if env.Type == "" {
		return nil
	}

	return &env
}

func extractVerdictFromEnvelope(content string) string {
	if env := extractEnvelope(content); env != nil && env.Verdict != "" {
		v := strings.ToLower(env.Verdict)
		if v == "approved" || v == "revise" {
			slog.Info("verdict extracted from response envelope", "verdict", v)
			return v
		}
	}

	return extractVerdict(content)
}
