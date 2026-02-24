package structuredoutput

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	jsonschemaLib "github.com/eino-contrib/jsonschema"
)

// claudeStrategy uses the tool-use-as-structured-output pattern.
// It defines a single tool whose parameters match the desired JSON schema,
// forces tool_choice to that tool, and extracts the result from the tool call arguments.
type claudeStrategy struct{}

const claudeToolName = "_respond"

func (c *claudeStrategy) generate(ctx context.Context, fn GenerateFn, messages []*schema.Message, s Schema) (*Result, error) {
	// Parse the raw JSON schema.
	var js jsonschemaLib.Schema
	if err := json.Unmarshal(s.Raw, &js); err != nil {
		return nil, fmt.Errorf("structuredoutput/claude: invalid schema: %w", err)
	}

	toolName := claudeToolName
	if s.Name != "" {
		toolName = s.Name
	}

	desc := s.Description
	if desc == "" {
		desc = "Return the structured result"
	}

	// Force the model to call this specific tool.
	opts := []model.Option{
		model.WithToolChoice(schema.ToolChoiceForced, toolName),
	}

	// Create a wrapper that binds the tool before calling.
	wrappedFn := func(ctx context.Context, msgs []*schema.Message, extraOpts ...model.Option) (*schema.Message, error) {
		allOpts := append(opts, extraOpts...)
		return fn(ctx, msgs, allOpts...)
	}

	// The caller's GenerateFn may not have tools bound. We need to ensure
	// the tool is visible. Append a system hint about the tool.
	augmented := make([]*schema.Message, len(messages))
	copy(augmented, messages)

	// Call the LLM. The model must support ToolCallingChatModel.
	// Note: The caller is responsible for ensuring the model has the tool bound
	// (via WithTools on the model). We pass tool_choice as an option.
	resp, err := wrappedFn(ctx, augmented)
	if err != nil {
		return nil, fmt.Errorf("structuredoutput/claude: generate: %w", err)
	}

	// Extract JSON from the tool call arguments.
	if len(resp.ToolCalls) == 0 {
		// Fallback: try to parse content as JSON directly.
		raw, parseErr := ExtractJSONFromContent(resp.Content)
		if parseErr != nil {
			return nil, fmt.Errorf("structuredoutput/claude: no tool calls in response and content is not JSON: %w", parseErr)
		}
		return &Result{JSON: raw, Usage: extractUsage(resp)}, nil
	}

	args := resp.ToolCalls[0].Function.Arguments
	if !json.Valid([]byte(args)) {
		return nil, fmt.Errorf("structuredoutput/claude: tool call arguments are not valid JSON")
	}

	return &Result{
		JSON:  json.RawMessage(args),
		Usage: extractUsage(resp),
	}, nil
}

// ToolInfo returns the schema tool definition for callers that need to bind
// it to a model before calling Generate. This is the tool that Claude will
// be forced to call.
func ToolInfo(s Schema) *schema.ToolInfo {
	var js jsonschemaLib.Schema
	json.Unmarshal(s.Raw, &js) //nolint: errcheck (validated at generate time)

	name := claudeToolName
	if s.Name != "" {
		name = s.Name
	}
	desc := s.Description
	if desc == "" {
		desc = "Return the structured result"
	}

	return &schema.ToolInfo{
		Name:        name,
		Desc:        desc,
		ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&js),
	}
}
