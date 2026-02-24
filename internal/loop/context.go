package loop

import (
	"encoding/json"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/message"
)

// LoopContext carries FSM routing metadata through agent-to-agent messages.
type LoopContext struct {
	Definition        LoopDefinition   `json:"definition"`
	State             LoopState        `json:"state"`
	TaskID            string           `json:"task_id"`
	Conductor         string           `json:"conductor"`
	OriginalTask      string           `json:"original_task"`
	UserRequest       string           `json:"user_request,omitempty"`
	DepParts          []map[string]any `json:"dep_parts,omitempty"`
	WorkSummary       string           `json:"work_summary,omitempty"`        // Latest working agent summary for terminal result.
	WorkArtifactURI   string           `json:"work_artifact_uri,omitempty"`   // artifacts/<worker-agent>/ for terminal pass-through
	WorkArtifactFiles []string         `json:"work_artifact_files,omitempty"` // cumulative worker artifact paths across revisions
	DispatchNonce     string           `json:"dispatch_nonce,omitempty"`
	DecisionContext   string           `json:"decision_context,omitempty"` // active project decisions for review enforcement
}

// EncodeContext wraps a LoopContext as a message DataPart with a "loop_context" key.
func EncodeContext(lc *LoopContext) message.DataPart {
	raw, _ := json.Marshal(lc)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return message.DataPart{Data: map[string]any{
		"loop_context": m,
	}}
}

// FilterDepParts reduces upstream dependency parts before message assembly.
// Two rules:
//   - Review states: exclude DepParts produced by the target agent (no self-reads).
//   - Revision after "revise" verdict: keep upstream DepParts, but compact them
//     so binding constraints remain visible without re-inlining large summaries.
//
// The full DepParts set is preserved in LoopContext so no information is permanently lost.
func FilterDepParts(depParts []map[string]any, def *LoopDefinition, nextState, verdict, nextAgent string) []map[string]any {
	if verdict == "revise" && !IsReviewState(def, nextState) {
		var compacted []map[string]any
		for _, dp := range depParts {
			agent, _ := dp["dependency_agent"].(string)
			if agent != "" && agent == nextAgent {
				continue
			}
			compacted = append(compacted, compactDepPart(dp))
		}
		return compacted
	}
	if IsReviewState(def, nextState) {
		var filtered []map[string]any
		for _, dp := range depParts {
			agent, _ := dp["dependency_agent"].(string)
			if agent != "" && agent == nextAgent {
				continue
			}
			filtered = append(filtered, dp)
		}
		return filtered
	}
	return depParts
}

func compactDepPart(dp map[string]any) map[string]any {
	out := make(map[string]any, len(dp))
	for k, v := range dp {
		out[k] = v
	}

	result, _ := out["result"].(string)
	if strings.TrimSpace(result) == "" {
		return out
	}

	limit := 800
	contextKind, _ := out["context_kind"].(string)
	if contextKind == "loop_feedback" {
		// Review feedback is critical routing context; keep enough detail for
		// actionable revision even when artifact URIs are present.
		limit = 1800
	} else if uri, _ := out["artifact_uri"].(string); strings.TrimSpace(uri) != "" {
		// When artifact files are available, keep only a short synopsis and
		// push the agent to inspect files directly.
		limit = 320
	}
	if len(result) <= limit {
		return out
	}

	out["result"] = result[:limit] + "\n[... truncated for revision context; use artifact files for authoritative details]"
	return out
}

// DecodeContext scans a message for a DataPart with a "loop_context" key and
// deserializes it into a LoopContext. Returns (nil, false) if not found.
func DecodeContext(msg *message.Message) (*LoopContext, bool) {
	for _, p := range msg.Parts {
		dp, ok := p.(message.DataPart)
		if !ok {
			continue
		}
		raw, ok := dp.Data["loop_context"]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var lc LoopContext
		if err := json.Unmarshal(b, &lc); err != nil {
			continue
		}
		return &lc, true
	}
	return nil, false
}
