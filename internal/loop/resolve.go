package loop

import "fmt"

// ResolveNextState determines the next FSM state based on the current state and verdict.
// For review states (those with condition-based transitions), it matches the verdict.
// For non-review states, it follows the single available transition.
func ResolveNextState(def *LoopDefinition, currentState, verdict string) (string, error) {
	// Find all transitions from the current state.
	var candidates []Transition
	for _, t := range def.Transitions {
		if t.From == currentState {
			candidates = append(candidates, t)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no transitions from state %q", currentState)
	}

	// If there's only one transition with no condition, follow it.
	if len(candidates) == 1 && candidates[0].Condition == "" {
		return candidates[0].To, nil
	}

	// Match verdict against conditions.
	if verdict != "" {
		for _, t := range candidates {
			if t.Condition == verdict {
				return t.To, nil
			}
		}
	}

	// No condition match -- if there's a single unconditional transition, use it.
	for _, t := range candidates {
		if t.Condition == "" {
			return t.To, nil
		}
	}

	return "", fmt.Errorf("no matching transition from state %q with verdict %q", currentState, verdict)
}

// IsReviewState returns true if the state has condition-based transitions (i.e., it's a review state).
func IsReviewState(def *LoopDefinition, state string) bool {
	for _, t := range def.Transitions {
		if t.From == state && t.Condition != "" {
			return true
		}
	}
	return false
}

// BuildStatePrompt builds a contextual prompt for a specific FSM state.
// decisionContext is an optional block of active project decisions injected into
// review states so reviewers enforce project-wide constraints.
func BuildStatePrompt(taskDesc string, def *LoopDefinition, state, lastVerdict, lastOutput, decisionContext string, workArtifactFiles []string) string {
	var b []byte

	b = append(b, taskDesc...)
	b = append(b, "\n\n"...)
	b = append(b, fmt.Sprintf("## Loop Context\nYou are in state %s of review loop %q.\n", state, def.ID)...)

	switch {
	case state == def.InitialState:
		b = append(b, "This is the initial work phase. Complete the task and submit your output.\n"...)
		b = append(b, "Optionally, include a JSON response envelope for structured metadata:\n"...)
		b = append(b, "```json\n{\"type\": \"task_result\", \"status\": \"success\", \"summary\": \"...\", \"artifacts\": [\"path/to/file\"]}\n```\n"...)
		if lastOutput != "" {
			b = append(b, "You are receiving context from a prior agent. Use it as input — produce YOUR OWN work, do not echo or summarize it.\n"...)
		}
	case lastVerdict == "revise":
		b = append(b, "Your previous work was reviewed and needs revision.\n"...)
		b = append(b, "Your files from the previous iteration are still on disk in $SCRATCH_DIR.\n"...)
		if len(workArtifactFiles) > 0 {
			b = append(b, "Files you produced in the previous iteration (available in $SCRATCH_DIR):\n"...)
			for _, f := range workArtifactFiles {
				b = append(b, fmt.Sprintf("  - %s\n", f)...)
			}
			b = append(b, "Do not waste a tool call listing these — they are already known.\n"...)
		}
		b = append(b, "Only modify files that the reviewer flagged — do not regenerate unchanged files.\n"...)
		b = append(b, "You MUST use ```file:path``` blocks to save any modifications you make.\n"...)
		b = append(b, "Address the review feedback below.\n"...)
		b = append(b, "The actionable review feedback is already included in this prompt under iterative loop feedback context.\n"...)
		b = append(b, "Do NOT search for a separate review.md file.\n"...)
		b = append(b, "Treat upstream dependency artifacts as source-of-truth constraints; reviewer feedback is advisory until verified.\n"...)
		b = append(b, "If reviewer feedback conflicts with upstream artifacts, preserve artifact fidelity and explicitly note the conflict with file references.\n"...)
		b = append(b, "Do not introduce new requirements that are absent from the original request and upstream dependencies.\n"...)
		b = append(b, "Upstream dependency artifacts have not changed since the initial work phase. Only re-read them if the reviewer questions specific values from those files.\n"...)
		b = append(b, "Re-read the flagged implementation files via shell to verify the reviewer's claims before responding.\n"...)
		b = append(b, "If the reviewer is correct, fix the issues. If the reviewer is wrong, cite the specific file contents that disprove their claim.\n"...)
		b = append(b, "Responding with \"no changes needed\" without re-reading files is not acceptable.\n"...)
	default:
		// Review state — list ALL possible verdicts explicitly.
		var verdicts []string
		for _, t := range def.Transitions {
			if t.From == state && t.Condition != "" {
				verdicts = append(verdicts, t.Condition)
			}
		}
		if len(verdicts) > 0 {
			b = append(b, "Review the work below. You MUST include your verdict using one of these formats:\n\n"...)
			b = append(b, "**Option A (preferred):** Include a JSON response envelope:\n"...)
			b = append(b, "```json\n{\"type\": \"verdict\", \"verdict\": \"approved|revise\", \"summary\": \"...\", \"issues\": [...]}\n```\n"...)
			b = append(b, "The issues array is for structured feedback (file, line, severity, message).\n\n"...)
			b = append(b, "**Option B:** Start your response with:\n"...)
			for _, v := range verdicts {
				b = append(b, fmt.Sprintf("  VERDICT: %s\n", v)...)
			}
			b = append(b, "\nEither format is accepted. The VERDICT line or JSON envelope must appear in your response.\n\n"...)
			b = append(b, "**IMPORTANT — approval bias:** Approve if the work is functionally correct and "...)
			b = append(b, "reasonably matches the spec, even if not pixel-perfect. Revision cycles are expensive. "...)
			b = append(b, "Only request revisions for issues a user would immediately notice as wrong or broken. "...)
			b = append(b, "Minor cosmetic differences (spacing, color shades, font weights) are NOT grounds for revision.\n\n"...)
			b = append(b, "Review against the artifact files provided as dependency context.\n"...)
			b = append(b, "Do not report 'missing implementation' if artifact files are listed — read them first.\n"...)
			b = append(b, "Cite specific file paths in your review feedback.\n"...)
			b = append(b, "Base findings on evidence from artifacts and upstream dependencies; do not invent requirements.\n"...)
			b = append(b, "For each major issue, cite both expected source artifact and observed implementation file.\n"...)
			guidelines := def.ReviewGuidelinesForState(state)
			if guidelines != "" {
				b = append(b, "Focus your review on these areas:\n"...)
				b = append(b, guidelines...)
				b = append(b, '\n')
			} else {
				b = append(b, "Check that the implementation is broadly correct:\n"...)
				b = append(b, "- Required features from the spec are present and functional\n"...)
				b = append(b, "- Layout structure matches the spec (correct component types, sections present)\n"...)
				b = append(b, "- No broken functionality (runtime errors, missing exports, broken imports)\n"...)
				b = append(b, "- Data model matches upstream specifications\n"...)
				b = append(b, "Do NOT flag minor cosmetic differences — approve if the work is functionally sound.\n"...)
			}
			if decisionContext != "" {
				b = append(b, '\n')
				b = append(b, decisionContext...)
				b = append(b, '\n')
				b = append(b, "Verify the implementation adheres to these active project decisions.\n"...)
			}
		}
	}

	return string(b)
}
