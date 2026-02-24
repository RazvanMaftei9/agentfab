package structuredoutput

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// promptStrategy is the fallback for unknown providers. It relies on system
// prompt engineering to get JSON output, then validates it.
type promptStrategy struct{}

func (p *promptStrategy) generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, s Schema) (*Result, error) {
	schemaHint := fmt.Sprintf("You MUST respond with valid JSON matching this schema:\n```json\n%s\n```\nRespond with ONLY the JSON object, no other text.", string(s.Raw))

	augmented := make([]*schema.Message, 0, len(messages)+1)
	augmented = append(augmented, schema.SystemMessage(schemaHint))
	augmented = append(augmented, messages...)

	resp, err := fn(ctx, augmented)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/prompt: generate: %w", err)
	}

	raw, err := ExtractJSONFromContent(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/prompt: %w", err)
	}

	return &Result{JSON: raw, Usage: extractUsage(resp)}, nil
}
