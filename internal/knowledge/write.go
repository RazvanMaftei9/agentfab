package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

type StorageHandle interface {
	StorageReader
	StorageWriter
}

// Apply writes knowledge documents to per-agent storage, merges the manifest into
// per-agent and shared graphs, prunes per-agent graphs, and persists both.
func Apply(
	ctx context.Context,
	storageFor func(string) StorageHandle,
	sharedStorage StorageHandle,
	manifest *Manifest,
	sharedGraph *Graph,
	requestID, requestName string,
	pruneOpts PruneOpts,
) error {
	if sharedGraph == nil {
		sharedGraph = NewGraph()
	}

	byAgent := make(map[string][]ManifestNode)
	for _, mn := range manifest.Nodes {
		byAgent[mn.Agent] = append(byAgent[mn.Agent], mn)
	}

	// Classify edges: intra-agent go to per-agent + shared; cross-agent go to shared only.
	nodeAgent := make(map[string]string, len(manifest.Nodes))
	for _, mn := range manifest.Nodes {
		nodeAgent[mn.ID] = mn.Agent
	}
	for id, n := range sharedGraph.Nodes {
		if _, ok := nodeAgent[id]; !ok {
			nodeAgent[id] = n.Agent
		}
	}

	type classifiedEdge struct {
		edge      Edge
		fromAgent string
		toAgent   string
	}
	var allEdges []classifiedEdge
	for _, e := range manifest.Edges {
		allEdges = append(allEdges, classifiedEdge{
			edge:      e,
			fromAgent: nodeAgent[e.From],
			toAgent:   nodeAgent[e.To],
		})
	}

	for agent, nodes := range byAgent {
		agentStore := storageFor(agent)

		for _, mn := range nodes {
			slug := mn.ID
			if i := strings.Index(slug, "/"); i >= 0 {
				slug = slug[i+1:]
			}
			docPath := fmt.Sprintf("docs/%s.md", slug)
			if err := agentStore.Write(ctx, runtime.TierAgent, docPath, []byte(mn.Content)); err != nil {
				slog.Warn("knowledge: failed to write doc", "agent", agent, "path", docPath, "error", err)
				continue
			}
			slog.Debug("knowledge: wrote doc", "agent", agent, "path", docPath)
		}

		agentGraph, err := LoadFromTier(ctx, agentStore, runtime.TierAgent)
		if err != nil {
			slog.Warn("knowledge: failed to load agent graph", "agent", agent, "error", err)
			agentGraph = NewGraph()
		}
		if agentGraph == nil {
			agentGraph = NewGraph()
		}

		agentManifest := &Manifest{Nodes: nodes}
		for _, ce := range allEdges {
			if ce.fromAgent == agent && ce.toAgent == agent {
				agentManifest.Edges = append(agentManifest.Edges, ce.edge)
			}
		}

		agentGraph.Merge(agentManifest, requestID, requestName)
		evicted := agentGraph.Prune(pruneOpts)
		if len(evicted) > 0 {
			slog.Debug("knowledge: pruned agent graph", "agent", agent, "evicted", len(evicted))
		}

		if err := SaveToTier(ctx, agentStore, runtime.TierAgent, agentGraph); err != nil {
			slog.Warn("knowledge: failed to save agent graph", "agent", agent, "error", err)
		}
	}

	sharedManifest := &Manifest{}
	for _, mn := range manifest.Nodes {
		sharedManifest.Nodes = append(sharedManifest.Nodes, ManifestNode{
			ID:         mn.ID,
			Agent:      mn.Agent,
			Title:      mn.Title,
			Summary:    mn.Summary,
			Tags:       mn.Tags,
			Confidence: mn.Confidence,
			Source:     mn.Source,
			TTLDays:    mn.TTLDays,
			// Content intentionally omitted — shared graph carries references only.
		})
	}
	sharedManifest.Edges = manifest.Edges

	sharedGraph.Merge(sharedManifest, requestID, requestName)

	if err := Save(ctx, sharedStorage, sharedGraph); err != nil {
		return fmt.Errorf("save shared knowledge graph: %w", err)
	}

	slog.Info("knowledge updated",
		"nodes", len(manifest.Nodes),
		"edges", len(manifest.Edges),
		"agents", len(byAgent),
		"graph_version", sharedGraph.Version,
	)
	return nil
}
