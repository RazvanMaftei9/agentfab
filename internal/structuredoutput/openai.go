package structuredoutput

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// openaiStrategy uses the native ResponseFormat with JSON schema.
// OpenAI models guarantee valid JSON matching the schema when using
// ChatCompletionResponseFormatTypeJSONSchema.
//
// Note: The ResponseFormat must be set on the model config at creation time
// (not as a per-call option in eino). For structured output calls, the caller
// should create a dedicated model instance with ResponseFormat configured.
// As a fallback, this strategy adds a JSON-mode system prompt and validates.
type openaiStrategy struct{}

func (o *openaiStrategy) generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, s Schema) (*Result, error) {
	// Add a system instruction to produce JSON matching the schema.
	// When the model is created with ResponseFormat=JSONSchema, this is
	// redundant but harmless. When it's not, this provides best-effort guidance.
	schemaHint := fmt.Sprintf("You MUST respond with valid JSON matching this schema:\n```json\n%s\n```\nRespond with ONLY the JSON object, no other text.", string(s.Raw))

	augmented := make([]*schema.Message, 0, len(messages)+1)
	augmented = append(augmented, schema.SystemMessage(schemaHint))
	augmented = append(augmented, messages...)

	resp, err := fn(ctx, augmented)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/openai: generate: %w", err)
	}

	raw, err := ExtractJSONFromContent(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/openai: %w", err)
	}

	return &Result{JSON: raw, Usage: extractUsage(resp)}, nil
}

// OpenAIResponseFormat returns the schema in the format needed for
// openai.ChatModelConfig.ResponseFormat. Callers can use this to create
// a model with native JSON schema enforcement.
func OpenAIResponseFormat(s Schema) map[string]any {
	name := s.Name
	if name == "" {
		name = "structured_output"
	}
	var schemaObj any
	json.Unmarshal(s.Raw, &schemaObj) //nolint: errcheck

	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   name,
			"strict": true,
			"schema": schemaObj,
		},
	}
}
