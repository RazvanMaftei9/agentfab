package structuredoutput

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// geminiStrategy uses system prompt guidance for JSON output.
// The Gemini eino-ext provider supports WithResponseJSONSchema on the config,
// but not as a per-call option. Like OpenAI, the caller should configure the
// model at creation time for true schema enforcement. This strategy provides
// best-effort prompt-based JSON generation with validation.
type geminiStrategy struct{}

func (g *geminiStrategy) generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, s Schema) (*Result, error) {
	schemaHint := fmt.Sprintf("You MUST respond with valid JSON matching this schema:\n```json\n%s\n```\nRespond with ONLY the JSON object, no other text.", string(s.Raw))

	augmented := make([]*schema.Message, 0, len(messages)+1)
	augmented = append(augmented, schema.SystemMessage(schemaHint))
	augmented = append(augmented, messages...)

	resp, err := fn(ctx, augmented)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/gemini: generate: %w", err)
	}

	raw, err := ExtractJSONFromContent(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/gemini: %w", err)
	}

	return &Result{JSON: raw, Usage: extractUsage(resp)}, nil
}
