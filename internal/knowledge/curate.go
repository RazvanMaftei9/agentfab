package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
	"github.com/razvanmaftei/agentfab/internal/structuredoutput"
)

const curatePrompt = `You are curating a knowledge graph for an AI agent named %q.

## Current Knowledge Graph (%d nodes)
%s

## Instructions

Produce a **smaller, consolidated** version of this knowledge graph. Your goals:
1. Merge semantically overlapping nodes into single, richer nodes
2. Remove redundant or obsolete information
3. Preserve ALL nodes tagged "decision" — these are critical constraints
4. Preserve ALL nodes with confidence >= 0.8 — these are high-value knowledge
5. Keep the graph well-connected: maintain edges between related concepts
6. Do NOT add new knowledge — only consolidate and reorganize existing nodes
7. Node IDs must follow the {agent}/{kebab-slug} convention

The curated graph must have FEWER nodes than the original. A good curation reduces nodes by 20-40%%.

Output ONLY JSON:
{
  "nodes": [
    {
      "id": "agent/slug",
      "agent": "agent-name",
      "title": "Human-Readable Title",
      "summary": "Consolidated one-paragraph summary.",
      "tags": ["tag1", "tag2"],
      "confidence": 0.9,
      "source": "task_result"
    }
  ],
  "edges": [
    {"from": "agent/slug-a", "to": "agent/slug-b", "relation": "depends_on"}
  ]
}`

type CurateResult struct {
	CuratedGraph *Graph
	NodesIn      int
	NodesOut     int
}

// Curate sends the agent's knowledge graph to the LLM for consolidation.
func Curate(
	ctx context.Context,
	generate func(context.Context, []*schema.Message) (*schema.Message, error),
	agentName string,
	graph *Graph,
	opts CurationOpts,
) (*CurateResult, error) {
	if graph == nil || len(graph.Nodes) < opts.threshold() {
		return nil, fmt.Errorf("graph too small for curation (%d nodes, threshold %d)", len(graph.Nodes), opts.threshold())
	}

	graphSummary := graph.Summary()
	systemMsg := fmt.Sprintf(curatePrompt, agentName, len(graph.Nodes), graphSummary)

	input := []*schema.Message{
		schema.SystemMessage(systemMsg),
		schema.UserMessage(fmt.Sprintf("Curate the knowledge graph for agent %q. Reduce %d nodes while preserving decisions and high-confidence knowledge.", agentName, len(graph.Nodes))),
	}

	resp, err := generate(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("curation LLM call: %w", err)
	}

	manifest, err := parseCurateManifest(resp.Content)
	if err != nil {
		return nil, fmt.Errorf("parse curation result: %w", err)
	}

	curated := NewGraph()
	for _, mn := range manifest.Nodes {
		slug := mn.ID
		if i := strings.Index(slug, "/"); i >= 0 {
			slug = slug[i+1:]
		}
		filePath := fmt.Sprintf("docs/%s.md", slug)

		node := &Node{
			ID:         mn.ID,
			Agent:      mn.Agent,
			Title:      mn.Title,
			FilePath:   filePath,
			Tags:       mn.Tags,
			Summary:    mn.Summary,
			Confidence: mn.Confidence,
			Source:     mn.Source,
		}

		if orig, ok := graph.Nodes[mn.ID]; ok {
			node.Hits = orig.Hits
			node.LastHitAt = orig.LastHitAt
			node.CreatedAt = orig.CreatedAt
			node.RequestID = orig.RequestID
			node.RequestName = orig.RequestName
			node.TTLDays = orig.TTLDays
			if node.FilePath == "" {
				node.FilePath = orig.FilePath
			}
		}

		curated.Nodes[mn.ID] = node
	}
	for _, e := range manifest.Edges {
		curated.Edges = append(curated.Edges, &Edge{
			From:     e.From,
			To:       e.To,
			Relation: e.Relation,
		})
	}

	return &CurateResult{
		CuratedGraph: curated,
		NodesIn:      len(graph.Nodes),
		NodesOut:     len(curated.Nodes),
	}, nil
}

type curateManifestNode struct {
	ID         string   `json:"id"`
	Agent      string   `json:"agent"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Tags       []string `json:"tags,omitempty"`
	Confidence float64  `json:"confidence,omitempty"`
	Source     string   `json:"source,omitempty"`
}

type curateManifest struct {
	Nodes []curateManifestNode `json:"nodes"`
	Edges []Edge               `json:"edges"`
}

func parseCurateManifest(content string) (*curateManifest, error) {
	rawJSON, err := structuredoutput.ExtractJSONFromContent(content)
	if err != nil {
		return nil, fmt.Errorf("extract JSON: %w", err)
	}
	var m curateManifest
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return nil, fmt.Errorf("unmarshal curation manifest: %w", err)
	}
	return &m, nil
}

// ValidateCurated checks that the curated graph preserves required invariants.
func ValidateCurated(original, curated *Graph) error {
	if original == nil || curated == nil {
		return fmt.Errorf("nil graph")
	}

	if len(curated.Nodes) > len(original.Nodes) {
		return fmt.Errorf("curated graph has more nodes (%d) than original (%d)",
			len(curated.Nodes), len(original.Nodes))
	}

	for id, node := range original.Nodes {
		if !node.HasTag("decision") || node.IsStale() || original.IsSuperseded(id) {
			continue
		}
		if _, ok := curated.Nodes[id]; !ok {
			return fmt.Errorf("decision node %q was removed by curation", id)
		}
	}

	for id, node := range original.Nodes {
		if node.Confidence < 0.8 || node.IsStale() || original.IsSuperseded(id) {
			continue
		}
		if _, ok := curated.Nodes[id]; !ok {
			return fmt.Errorf("high-confidence node %q (%.1f) was removed by curation", id, node.Confidence)
		}
	}

	for _, e := range curated.Edges {
		if _, ok := curated.Nodes[e.From]; !ok {
			return fmt.Errorf("dangling edge: from %q does not exist in curated graph", e.From)
		}
		if _, ok := curated.Nodes[e.To]; !ok {
			return fmt.Errorf("dangling edge: to %q does not exist in curated graph", e.To)
		}
	}

	return nil
}

// SwapCurated replaces the active graph with the curated version, moving removed nodes to cold storage.
func SwapCurated(ctx context.Context, storage StorageHandle, original, curated *Graph, coldOpts ColdStorageOpts) error {
	coldGraph, err := LoadCold(ctx, storage)
	if err != nil {
		coldGraph = NewGraph()
	}
	if coldGraph == nil {
		coldGraph = NewGraph()
	}

	now := time.Now()
	for id, node := range original.Nodes {
		if _, ok := curated.Nodes[id]; ok {
			continue // still active
		}
		coldNode := *node
		coldNode.UpdatedAt = now
		coldGraph.Nodes[id] = &coldNode

		if node.FilePath != "" {
			data, readErr := storage.Read(ctx, runtime.TierAgent, node.FilePath)
			if readErr == nil {
				coldPath := "cold_storage/" + node.FilePath
				_ = storage.Write(ctx, runtime.TierAgent, coldPath, data)
			}
		}
	}

	removedSet := make(map[string]bool)
	for id := range original.Nodes {
		if _, ok := curated.Nodes[id]; !ok {
			removedSet[id] = true
		}
	}
	for _, e := range original.Edges {
		if removedSet[e.From] || removedSet[e.To] {
			coldGraph.Edges = append(coldGraph.Edges, e)
		}
	}

	purgeColdExpired(coldGraph, coldOpts.retentionDays())

	if err := SaveCold(ctx, storage, coldGraph); err != nil {
		return fmt.Errorf("save cold graph during swap: %w", err)
	}

	curated.Version = original.Version + 1

	return nil
}
