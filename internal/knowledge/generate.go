package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/message"
	"github.com/razvanmaftei/agentfab/internal/structuredoutput"
	"github.com/razvanmaftei/agentfab/internal/taskgraph"
)

const generatePrompt = `You are analyzing completed task results to extract persistent knowledge for an AgentFab fabric.

## Completed Tasks
%s

## Existing Knowledge Graph
%s

## Instructions

Produce a JSON manifest of knowledge nodes and edges. Each node is a self-contained concept that will be saved as a markdown document in the owning agent's docs directory.

Rules:
- One concept per node. Keep nodes focused and atomic.
- Node ID format: {agent}/{kebab-slug} (e.g., "agent-a/api-design")
- Reuse existing node IDs to update rather than duplicate
- Each node needs a summary (one paragraph, used for context injection) and content (full markdown doc)
- Tags should be specific: "auth", "database", "api", "frontend", etc.
- Edge relations: depends_on, implements, related_to, supersedes, refines
- Create edges liberally — connect new nodes to existing ones, not just to each other
- If node A depends_on B, also consider whether B related_to A or other reverse connections
- Cross-agent edges are encouraged: upstream agent decisions connect to implementing agent outputs
- Only create nodes for substantive knowledge worth preserving across requests
- Skip trivial or purely procedural information
- For each node, assign a confidence score (0.0-1.0) based on how certain you are about the information
- Set source to "task_result" for facts directly from task output, "inferred" for conclusions you drew
- Set ttl_days to 0 for stable facts (no expiry), 30-90 for time-sensitive conclusions
- When a task establishes or changes a technology choice, design system, framework, or architectural pattern that affects future work, create a "decision" node. Decision nodes MUST include "decision" in their tags. The summary should state the constraint as a clear rule (e.g., "TaskMaster uses Material Design 3 as its design system — all UI work must follow MD3 guidelines"). Use "supersedes" edges to obsolete prior decisions on the same topic. Set ttl_days to 0 — decisions persist until explicitly changed

Output ONLY JSON:
{
  "nodes": [
    {
      "id": "agent-name/slug",
      "agent": "agent-name",
      "title": "Human-Readable Title",
      "summary": "One paragraph summary for quick context injection.",
      "content": "# Title\n\nFull markdown document content...",
      "tags": ["tag1", "tag2"],
      "confidence": 0.9,
      "source": "task_result",
      "ttl_days": 0
    }
  ],
  "edges": [
    {"from": "agent-a/slug-a", "to": "agent-b/slug-b", "relation": "depends_on"}
  ]
}`

// Generate calls the LLM to produce a knowledge manifest from task results.
func Generate(
	ctx context.Context,
	generate func(context.Context, []*schema.Message) (*schema.Message, error),
	graph *taskgraph.TaskGraph,
	existingGraph *Graph,
	requestName string,
) (*Manifest, *message.TokenUsage, error) {
	var tasks strings.Builder
	for _, t := range graph.Tasks {
		if t.Status != taskgraph.StatusCompleted || t.Result == "" {
			continue
		}
		result := t.Result
		if len(result) > 4000 {
			result = result[:4000] + "\n\n[... truncated]"
		}
		tasks.WriteString(fmt.Sprintf("### %s (%s)\n%s\n\n%s\n\n", t.ID, t.Agent, t.Description, result))
	}

	if tasks.Len() == 0 {
		return &Manifest{}, nil, nil
	}

	graphSummary := "No existing knowledge."
	if existingGraph != nil {
		if s := existingGraph.Summary(); s != "" {
			graphSummary = s
		}
	}

	systemMsg := fmt.Sprintf(generatePrompt, tasks.String(), graphSummary)

	input := []*schema.Message{
		schema.SystemMessage(systemMsg),
		schema.UserMessage(fmt.Sprintf("Extract knowledge from the completed request %q.", requestName)),
	}

	resp, err := generate(ctx, input)
	if err != nil {
		return nil, nil, fmt.Errorf("knowledge generation LLM call: %w", err)
	}

	manifest, err := parseManifest(resp.Content)
	if err != nil {
		return nil, nil, err
	}

	var usage *message.TokenUsage
	if resp.ResponseMeta != nil && resp.ResponseMeta.Usage != nil {
		u := resp.ResponseMeta.Usage
		usage = &message.TokenUsage{
			InputTokens:  int64(u.PromptTokens),
			OutputTokens: int64(u.CompletionTokens),
			TotalTokens:  int64(u.PromptTokens + u.CompletionTokens),
		}
	}

	return manifest, usage, nil
}

func parseManifest(content string) (*Manifest, error) {
	rawJSON, err := structuredoutput.ExtractJSONFromContent(content)
	if err != nil {
		return nil, fmt.Errorf("parse knowledge manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return nil, fmt.Errorf("parse knowledge manifest: %w", err)
	}
	return &m, nil
}
