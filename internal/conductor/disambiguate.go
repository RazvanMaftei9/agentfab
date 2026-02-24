package conductor

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/structuredoutput"
)

const disambiguatePrompt = `You are the Conductor of an AgentFab fabric. Before decomposing a user request into tasks, evaluate whether the request is specific enough for agents to produce correct output.

Available agents:
%s

Evaluate: "If I hand this request to agents as-is, will they know exactly what to build?"

Consider:
- Are there ambiguous scope boundaries (e.g., "decoupled" but unclear how)?
- Are target platforms or deployment constraints missing when they matter?
- Are there multiple reasonable interpretations of what the user wants?
- Are key features mentioned but underspecified?

Do NOT flag:
- Implementation details agents decide themselves (CSS framework, state management, folder structure)
- Standard technology choices that agents handle
- Requests that are already concrete and specific

Respond with JSON only:
{"clear": true}
or
{"clear": false, "question": "Single concise question with concrete options when possible"}`

type DisambiguateResult struct {
	Clear      bool
	Question   string
	TokenUsage *message.TokenUsage
}

type disambiguateJSON struct {
	Clear    bool   `json:"clear"`
	Question string `json:"question,omitempty"`
}

// Disambiguate checks if a request is specific enough for decomposition.
func Disambiguate(ctx context.Context, generate func(context.Context, []*schema.Message) (*schema.Message, error), userRequest string, agentRoster []string) (*DisambiguateResult, error) {
	rosterDesc := ""
	for _, name := range agentRoster {
		rosterDesc += fmt.Sprintf("- %s\n", name)
	}

	prompt := fmt.Sprintf(disambiguatePrompt, rosterDesc)

	input := []*schema.Message{
		schema.SystemMessage(prompt),
		schema.UserMessage(userRequest),
	}

	resp, err := generate(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("disambiguate LLM call: %w", err)
	}

	rawJSON, err := structuredoutput.ExtractJSONFromContent(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("parse disambiguate result: %w", err)
	}

	var raw disambiguateJSON
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse disambiguate JSON: %w", err)
	}

	result := &DisambiguateResult{
		Clear:    raw.Clear,
		Question: raw.Question,
	}

	if resp.ResponseMeta != nil && resp.ResponseMeta.Usage != nil {
		u := resp.ResponseMeta.Usage
		result.TokenUsage = &message.TokenUsage{
			InputTokens:  int64(u.PromptTokens),
			OutputTokens: int64(u.CompletionTokens),
			TotalTokens:  int64(u.PromptTokens + u.CompletionTokens),
		}
	}

	return result, nil
}
