// Package structuredoutput provides model-agnostic structured JSON output
// from LLMs. Each provider uses a different mechanism:
//   - OpenAI/OpenAI-compatible: native ResponseFormat with JSON schema
//   - Claude: tool-use-as-structured-output (forced tool choice)
//   - Gemini: response JSON schema option
//
// The package exposes a single Generate function that selects the right
// strategy based on the provider prefix in the model ID.
package structuredoutput

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/llm"
)

// Schema describes the expected JSON output shape. It wraps a raw JSON schema
// definition that can be passed to each provider's native mechanism.
type Schema struct {
	Name        string          // Human-readable name (used for tool name / schema title)
	Description string          // One-line description
	Raw         json.RawMessage // JSON Schema bytes ({"type":"object","properties":{...}})
}

// Result holds the parsed structured output and metadata.
type Result struct {
	JSON  json.RawMessage // The validated JSON output
	Usage *Usage          // Optional token usage
}

// Usage tracks token consumption for the structured output call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// GenerateFn is the function signature for calling an LLM. It matches the
// pattern used throughout AgentFab (agent, conductor, knowledge).
type GenerateFn func(context.Context, []*schema.Message, ...model.Option) (*schema.Message, error)

// Generate calls the LLM with the appropriate structured output strategy for
// the given provider and returns validated JSON. The modelID must use
// "provider/model-id" format (e.g., "anthropic/claude-opus-4").
func Generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, modelID string, s Schema) (*Result, error) {
	provider, _, err := llm.ParseModelID(modelID)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput: %w", err)
	}

	var strategy strategy
	switch provider {
	case "anthropic":
		strategy = &claudeStrategy{}
	case "openai", "openai-compat":
		strategy = &openaiStrategy{}
	case "google":
		strategy = &geminiStrategy{}
	default:
		// Fallback: prompt-based with JSON validation (no schema enforcement).
		strategy = &promptStrategy{}
	}

	return strategy.generate(ctx, fn, messages, s)
}

// strategy is the internal interface each provider implements.
type strategy interface {
	generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, s Schema) (*Result, error)
}

// extractUsage pulls token usage from a schema.Message response.
func extractUsage(resp *schema.Message) *Usage {
	if resp == nil || resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return nil
	}
	u := resp.ResponseMeta.Usage
	return &Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
}
