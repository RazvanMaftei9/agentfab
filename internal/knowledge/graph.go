package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

const graphPath = "knowledge/graph.json"

type StorageReader interface {
	Read(context.Context, runtime.StorageTier, string) ([]byte, error)
	Exists(context.Context, runtime.StorageTier, string) (bool, error)
}

type StorageWriter interface {
	Write(context.Context, runtime.StorageTier, string, []byte) error
}

func parseGraph(data []byte) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parse knowledge graph: %w", err)
	}
	if g.Nodes == nil {
		g.Nodes = make(map[string]*Node)
	}
	return &g, nil
}

func marshalGraph(g *Graph) ([]byte, error) {
	return json.MarshalIndent(g, "", "  ")
}

func NewGraph() *Graph {
	return &Graph{
		Version: 1,
		Nodes:   make(map[string]*Node),
	}
}

// LoadFromTier reads the knowledge graph from a storage tier. Returns nil, nil if not found.
func LoadFromTier(ctx context.Context, storage StorageReader, tier runtime.StorageTier) (*Graph, error) {
	exists, err := storage.Exists(ctx, tier, graphPath)
	if err != nil || !exists {
		return nil, err
	}

	data, err := storage.Read(ctx, tier, graphPath)
	if err != nil {
		return nil, fmt.Errorf("read knowledge graph: %w", err)
	}

	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parse knowledge graph: %w", err)
	}
	if g.Nodes == nil {
		g.Nodes = make(map[string]*Node)
	}
	return &g, nil
}

// Load reads the knowledge graph from shared storage. Returns nil, nil if not found.
func Load(ctx context.Context, storage StorageReader) (*Graph, error) {
	return LoadFromTier(ctx, storage, runtime.TierShared)
}

// LoadGraphFromFile reads graph.json from a file path. Returns an empty graph if not found.
func LoadGraphFromFile(path string) (*Graph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewGraph(), nil
		}
		return nil, fmt.Errorf("read knowledge graph: %w", err)
	}

	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parse knowledge graph: %w", err)
	}
	if g.Nodes == nil {
		g.Nodes = make(map[string]*Node)
	}
	return &g, nil
}

func SaveToTier(ctx context.Context, storage StorageWriter, tier runtime.StorageTier, g *Graph) error {
	g.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal knowledge graph: %w", err)
	}
	return storage.Write(ctx, tier, graphPath, data)
}

func Save(ctx context.Context, storage StorageWriter, g *Graph) error {
	return SaveToTier(ctx, storage, runtime.TierShared, g)
}

// FindByTagAndSummary returns the ID of an existing node with the given tag and summary, or "".
func (g *Graph) FindByTagAndSummary(tag, summary string) string {
	for id, node := range g.Nodes {
		if node.Summary == summary && node.HasTag(tag) {
			return id
		}
	}
	return ""
}

// Merge upserts manifest nodes into the graph and deduplicates edges.
func (g *Graph) Merge(manifest *Manifest, requestID, requestName string) {
	now := time.Now()

	for _, node := range manifest.Nodes {
		slug := node.ID
		if i := strings.Index(slug, "/"); i >= 0 {
			slug = slug[i+1:]
		}
		filePath := fmt.Sprintf("docs/%s.md", slug)

		existing, ok := g.Nodes[node.ID]
		if ok {
			existing.Title = node.Title
			existing.Summary = node.Summary
			existing.Tags = node.Tags
			existing.FilePath = filePath
			existing.RequestID = requestID
			existing.RequestName = requestName
			existing.UpdatedAt = now
			if node.Confidence > existing.Confidence || existing.Confidence == 0 {
				existing.Confidence = node.Confidence
			}
			if node.Source != "" {
				existing.Source = node.Source
			}
			if node.TTLDays > 0 {
				existing.TTLDays = node.TTLDays
			}
		} else {
			g.Nodes[node.ID] = &Node{
				ID:          node.ID,
				Agent:       node.Agent,
				Title:       node.Title,
				FilePath:    filePath,
				Tags:        node.Tags,
				RequestID:   requestID,
				RequestName: requestName,
				CreatedAt:   now,
				UpdatedAt:   now,
				Summary:     node.Summary,
				Confidence:  node.Confidence,
				Source:      node.Source,
				TTLDays:     node.TTLDays,
			}
		}
	}

	edgeSet := make(map[string]bool, len(g.Edges))
	for _, e := range g.Edges {
		edgeSet[edgeKey(e)] = true
	}
	for _, e := range manifest.Edges {
		key := edgeKey(&e)
		if !edgeSet[key] {
			edgeSet[key] = true
			g.Edges = append(g.Edges, &Edge{
				From:     e.From,
				To:       e.To,
				Relation: e.Relation,
			})
		}
	}

	g.Version++
}

func edgeKey(e *Edge) string {
	return e.From + "|" + e.Relation + "|" + e.To
}

// IsSuperseded returns true if a non-stale node supersedes this one via a "supersedes" edge.
func (g *Graph) IsSuperseded(nodeID string) bool {
	for _, e := range g.Edges {
		if e.To == nodeID && e.Relation == "supersedes" {
			if sup, ok := g.Nodes[e.From]; ok && !sup.IsStale() {
				return true
			}
		}
	}
	return false
}

// DecisionConflict describes two active decision nodes that share a domain tag.
type DecisionConflict struct {
	Tag    string // shared domain tag
	NodeA  string // first node ID
	NodeB  string // second node ID
	AgentA string
	AgentB string
}

// DetectDecisionConflicts finds active decision nodes sharing domain tags.
func (g *Graph) DetectDecisionConflicts() []DecisionConflict {
	type decisionEntry struct {
		id   string
		node *Node
	}
	var decisions []decisionEntry
	for id, n := range g.Nodes {
		if n.IsStale() || g.IsSuperseded(id) || !n.HasTag("decision") {
			continue
		}
		decisions = append(decisions, decisionEntry{id, n})
	}

	tagToNodes := make(map[string][]decisionEntry)
	for _, d := range decisions {
		for _, tag := range d.node.DomainTags() {
			tagToNodes[tag] = append(tagToNodes[tag], d)
		}
	}

	var conflicts []DecisionConflict
	seen := make(map[string]bool) // "nodeA|nodeB" dedup
	for tag, entries := range tagToNodes {
		if len(entries) < 2 {
			continue
		}
		for i := 0; i < len(entries); i++ {
			for j := i + 1; j < len(entries); j++ {
				key := entries[i].id + "|" + entries[j].id
				if seen[key] {
					continue
				}
				seen[key] = true
				conflicts = append(conflicts, DecisionConflict{
					Tag:    tag,
					NodeA:  entries[i].id,
					NodeB:  entries[j].id,
					AgentA: entries[i].node.Agent,
					AgentB: entries[j].node.Agent,
				})
			}
		}
	}
	return conflicts
}

// ActiveDecisions returns active decision nodes sorted by UpdatedAt descending.
func (g *Graph) ActiveDecisions() []*Node {
	var decisions []*Node
	for id, n := range g.Nodes {
		if n.IsStale() || g.IsSuperseded(id) || !n.HasTag("decision") {
			continue
		}
		decisions = append(decisions, n)
	}
	sort.Slice(decisions, func(i, j int) bool {
		return decisions[i].UpdatedAt.After(decisions[j].UpdatedAt)
	})
	return decisions
}

// RecentUserRequests returns active "user-request" nodes, most recent first, capped at max.
func (g *Graph) RecentUserRequests(max int) []*Node {
	var requests []*Node
	for id, n := range g.Nodes {
		if n.IsStale() || g.IsSuperseded(id) || !n.HasTag("user-request") {
			continue
		}
		requests = append(requests, n)
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].UpdatedAt.After(requests[j].UpdatedAt)
	})
	if max > 0 && len(requests) > max {
		requests = requests[:max]
	}
	return requests
}

// Query returns nodes for the given agents.
func (g *Graph) Query(agents []string) []*Node {
	agentSet := make(map[string]bool, len(agents))
	for _, name := range agents {
		agentSet[name] = true
	}

	var result []*Node
	for _, node := range g.Nodes {
		if agentSet[node.Agent] {
			result = append(result, node)
		}
	}
	return result
}

// Summary returns a text representation of all nodes for prompt injection.
func (g *Graph) Summary() string {
	if len(g.Nodes) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Existing knowledge nodes:\n")
	for _, node := range g.Nodes {
		b.WriteString(fmt.Sprintf("- %s: %s", node.ID, node.Title))
		if len(node.Tags) > 0 {
			b.WriteString(fmt.Sprintf(" [%s]", strings.Join(node.Tags, ", ")))
		}
		if node.RequestName != "" {
			b.WriteString(fmt.Sprintf(" (from request: %q)", node.RequestName))
		}
		b.WriteByte('\n')
		if node.Summary != "" {
			b.WriteString(fmt.Sprintf("  Summary: %s\n", node.Summary))
		}
	}
	return b.String()
}
