package knowledge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

const coldGraphPath = "cold_storage/graph.json"

// LoadCold reads the cold storage graph. Returns nil, nil if not found.
func LoadCold(ctx context.Context, storage StorageReader) (*Graph, error) {
	exists, err := storage.Exists(ctx, runtime.TierAgent, coldGraphPath)
	if err != nil || !exists {
		return nil, err
	}
	data, err := storage.Read(ctx, runtime.TierAgent, coldGraphPath)
	if err != nil {
		return nil, fmt.Errorf("read cold graph: %w", err)
	}
	return parseGraph(data)
}

func SaveCold(ctx context.Context, storage StorageWriter, g *Graph) error {
	g.UpdatedAt = time.Now()
	data, err := marshalGraph(g)
	if err != nil {
		return fmt.Errorf("marshal cold graph: %w", err)
	}
	return storage.Write(ctx, runtime.TierAgent, coldGraphPath, data)
}

// MoveToCold evicts cold-eligible nodes from the active graph to cold storage,
// relocates docs, and purges expired cold nodes past retention.
func MoveToCold(ctx context.Context, storage StorageHandle, activeGraph *Graph, opts ColdStorageOpts) (moved []string, purged int, err error) {
	minHits := opts.minHits()
	coldDays := opts.coldDays()
	retentionDays := opts.retentionDays()

	coldGraph, err := LoadCold(ctx, storage)
	if err != nil {
		return nil, 0, fmt.Errorf("load cold graph: %w", err)
	}
	if coldGraph == nil {
		coldGraph = NewGraph()
	}

	// Decision-tagged nodes always stay active to guide future work.
	now := time.Now()
	for id, node := range activeGraph.Nodes {
		if node.HasTag("decision") {
			continue
		}
		if !node.IsCold(minHits, coldDays) {
			continue
		}
		coldNode := *node
		coldNode.UpdatedAt = now
		coldGraph.Nodes[id] = &coldNode
		moved = append(moved, id)

		if node.FilePath != "" {
			data, readErr := storage.Read(ctx, runtime.TierAgent, node.FilePath)
			if readErr == nil {
				coldPath := "cold_storage/" + node.FilePath
				_ = storage.Write(ctx, runtime.TierAgent, coldPath, data)
			}
		}

		delete(activeGraph.Nodes, id)
	}

	if len(moved) > 0 {
		movedSet := make(map[string]bool, len(moved))
		for _, id := range moved {
			movedSet[id] = true
		}

		var activeEdges []*Edge
		for _, e := range activeGraph.Edges {
			fromCold := movedSet[e.From]
			toCold := movedSet[e.To]
			if fromCold || toCold {
				coldGraph.Edges = append(coldGraph.Edges, e)
			}
			if !fromCold && !toCold {
				activeEdges = append(activeEdges, e)
			}
		}
		activeGraph.Edges = activeEdges
	}

	purged = purgeColdExpired(coldGraph, retentionDays)

	if err := SaveCold(ctx, storage, coldGraph); err != nil {
		return moved, purged, fmt.Errorf("save cold graph: %w", err)
	}

	return moved, purged, nil
}

// RestoreFromCold moves specific nodes from the cold graph back to the active graph.
func RestoreFromCold(ctx context.Context, storage StorageHandle, activeGraph, coldGraph *Graph, nodeIDs []string) error {
	if coldGraph == nil || activeGraph == nil {
		return fmt.Errorf("nil graph")
	}

	restoredSet := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		node, ok := coldGraph.Nodes[id]
		if !ok {
			continue
		}
		activeGraph.Nodes[id] = node
		delete(coldGraph.Nodes, id)
		restoredSet[id] = true

		if node.FilePath != "" {
			coldPath := "cold_storage/" + node.FilePath
			data, readErr := storage.Read(ctx, runtime.TierAgent, coldPath)
			if readErr == nil {
				_ = storage.Write(ctx, runtime.TierAgent, node.FilePath, data)
			}
		}
	}

	if len(restoredSet) == 0 {
		return nil
	}

	var remainingColdEdges []*Edge
	for _, e := range coldGraph.Edges {
		fromRestored := restoredSet[e.From]
		toRestored := restoredSet[e.To]
		if fromRestored || toRestored {
				if activeGraph.Nodes[e.From] != nil && activeGraph.Nodes[e.To] != nil {
				activeGraph.Edges = append(activeGraph.Edges, e)
			}
		}
		if !fromRestored && !toRestored {
			remainingColdEdges = append(remainingColdEdges, e)
		}
	}
	coldGraph.Edges = remainingColdEdges

	return nil
}

// LookupCold performs keyword-based search over cold storage, returning up to maxResults.
func LookupCold(coldGraph *Graph, query string, maxResults int) []*Node {
	if coldGraph == nil || len(coldGraph.Nodes) == 0 || query == "" {
		return nil
	}
	if maxResults <= 0 {
		maxResults = 10
	}

	keywords := tokenize(query)
	if len(keywords) == 0 {
		return nil
	}

	type scored struct {
		node  *Node
		score float64
	}
	var matches []scored
	for _, node := range coldGraph.Nodes {
		score := keywordOverlap(node, keywords)
		if score > 0.05 {
			matches = append(matches, scored{node: node, score: score})
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})

	if len(matches) > maxResults {
		matches = matches[:maxResults]
	}

	result := make([]*Node, len(matches))
	for i, m := range matches {
		result[i] = m.node
	}
	return result
}

// purgeColdExpired removes cold nodes older than retentionDays.
func purgeColdExpired(g *Graph, retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	threshold := time.Duration(retentionDays) * 24 * time.Hour
	var purged int
	for id, node := range g.Nodes {
		if time.Since(node.UpdatedAt) > threshold {
			delete(g.Nodes, id)
			purged++
		}
	}

	if purged > 0 {
		clean := make([]*Edge, 0, len(g.Edges))
		for _, e := range g.Edges {
			if g.Nodes[e.From] != nil && g.Nodes[e.To] != nil {
				clean = append(clean, e)
			}
		}
		g.Edges = clean
	}

	return purged
}

func FormatColdResults(nodes []*Node) string {
	if len(nodes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Archived Knowledge (Cold Storage)\n")
	b.WriteString("The following knowledge was archived during curation. It may still be relevant:\n\n")
	for _, n := range nodes {
		b.WriteString(fmt.Sprintf("- **%s**: %s", n.Title, n.Summary))
		if n.RequestName != "" {
			b.WriteString(fmt.Sprintf(" *(from: %s)*", n.RequestName))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
