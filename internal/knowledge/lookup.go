package knowledge

import (
	"sort"
	"strings"
	"unicode"
)

type LookupOpts struct {
	MaxDepth int // max edge hops from seed nodes (default 2)
	MaxNodes int // max related nodes to return (default 15)
}

func (o LookupOpts) maxDepth() int {
	if o.MaxDepth <= 0 {
		return 2
	}
	return o.MaxDepth
}

func (o LookupOpts) maxNodes() int {
	if o.MaxNodes <= 0 {
		return 15
	}
	return o.MaxNodes
}

type LookupResult struct {
	Own     []*Node        // nodes owned by the agent
	Related []*RelatedNode // nodes from other agents reachable via edges
}

type RelatedNode struct {
	*Node
	Depth     int      // hop distance from seed set
	Relations []string // edge relation types that connected this node
	Score     float64  // relevance score (higher = more relevant)
}

type adjacencyEntry struct {
	NodeID   string
	Relation string
}

// Lookup performs BFS from the agent's own nodes, returning related nodes scored by depth and keyword overlap.
func Lookup(g *Graph, agent string, taskDesc string, opts LookupOpts) LookupResult {
	var result LookupResult
	if g == nil {
		return result
	}

	keywords := tokenize(taskDesc)
	seeds := make(map[string]bool)

	for id, n := range g.Nodes {
		if n.Agent == agent {
			if n.IsStale() || g.IsSuperseded(id) {
				continue
			}
			result.Own = append(result.Own, n)
			seeds[id] = true
		}
	}

	addKeywordSeeds(g, keywords, seeds)

	if len(seeds) == 0 {
		return result
	}

	result.Related = bfsRelated(g, seeds, agent, keywords, opts)
	recordHits(&result)
	return result
}

// LookupDual uses agentGraph for own nodes and sharedGraph for cross-agent BFS.
// Falls back to single-graph Lookup if agentGraph is nil.
func LookupDual(agentGraph, sharedGraph *Graph, agent, taskDesc string, opts LookupOpts) LookupResult {
	if agentGraph == nil {
		return Lookup(sharedGraph, agent, taskDesc, opts)
	}

	var result LookupResult
	keywords := tokenize(taskDesc)

	for id, n := range agentGraph.Nodes {
		if n.Agent == agent && !n.IsStale() && !agentGraph.IsSuperseded(id) {
			result.Own = append(result.Own, n)
		}
	}

	if sharedGraph == nil || (len(sharedGraph.Edges) == 0 && len(keywords) == 0) {
		recordHits(&result)
		return result
	}

	seeds := make(map[string]bool)
	for id, n := range sharedGraph.Nodes {
		if n.Agent == agent && !n.IsStale() && !sharedGraph.IsSuperseded(id) {
			seeds[id] = true
		}
	}
	addKeywordSeeds(sharedGraph, keywords, seeds)

	if len(seeds) == 0 {
		recordHits(&result)
		return result
	}

	result.Related = bfsRelated(sharedGraph, seeds, agent, keywords, opts)
	recordHits(&result)
	return result
}

// addKeywordSeeds adds up to 5 top keyword-matching nodes into the seed set.
func addKeywordSeeds(g *Graph, keywords map[string]bool, seeds map[string]bool) {
	if len(keywords) == 0 {
		return
	}
	var matches []struct {
		id    string
		score float64
	}
	for id, n := range g.Nodes {
		if n.IsStale() || g.IsSuperseded(id) || seeds[id] {
			continue
		}
		score := keywordScore(n, keywords)
		if score > 0.1 {
			matches = append(matches, struct {
				id    string
				score float64
			}{id, score})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	for i := 0; i < 5 && i < len(matches); i++ {
		seeds[matches[i].id] = true
	}
}

type bfsEntry struct {
	nodeID string
	depth  int
}

type bfsDiscovered struct {
	nodeID string
	depth  int
}

// bfsRelated performs BFS from seeds, scoring and capping related nodes (excludes agent's own).
func bfsRelated(g *Graph, seeds map[string]bool, agent string, keywords map[string]bool, opts LookupOpts) []*RelatedNode {
	adj := buildAdjacency(g.Edges)
	maxDepth := opts.maxDepth()

	visited := make(map[string]bool, len(seeds))
	for id := range seeds {
		visited[id] = true
	}

	queue := make([]bfsEntry, 0, len(seeds)*2)
	for id := range seeds {
		queue = append(queue, bfsEntry{nodeID: id, depth: 0})
	}

	var found []bfsDiscovered
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.depth >= maxDepth {
			continue
		}
		for _, neighbor := range adj[cur.nodeID] {
			if visited[neighbor.NodeID] {
				continue
			}
			node, exists := g.Nodes[neighbor.NodeID]
			if !exists || node.IsStale() || g.IsSuperseded(neighbor.NodeID) {
				continue
			}
			visited[neighbor.NodeID] = true
			nextDepth := cur.depth + 1
			found = append(found, bfsDiscovered{nodeID: neighbor.NodeID, depth: nextDepth})
			queue = append(queue, bfsEntry{nodeID: neighbor.NodeID, depth: nextDepth})
		}
	}

	for id := range seeds {
		node := g.Nodes[id]
		if node != nil && node.Agent != agent {
			found = append(found, bfsDiscovered{nodeID: id, depth: 0})
		}
	}

	related := make([]*RelatedNode, 0, len(found))
	for _, f := range found {
		node := g.Nodes[f.nodeID]
		if node.Agent == agent {
			continue
		}
		relations := collectRelations(g.Edges, seeds, visited, f.nodeID)
		score := computeScore(node, f.depth, keywords)
		related = append(related, &RelatedNode{
			Node:      node,
			Depth:     f.depth,
			Relations: relations,
			Score:     score,
		})
	}

	sort.Slice(related, func(i, j int) bool {
		return related[i].Score > related[j].Score
	})
	if maxN := opts.maxNodes(); len(related) > maxN {
		related = related[:maxN]
	}
	return related
}

func recordHits(result *LookupResult) {
	for _, n := range result.Own {
		n.RecordHit()
	}
	for _, rn := range result.Related {
		rn.RecordHit()
	}
}

func buildAdjacency(edges []*Edge) map[string][]adjacencyEntry {
	adj := make(map[string][]adjacencyEntry, len(edges)*2)
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], adjacencyEntry{NodeID: e.To, Relation: e.Relation})
		adj[e.To] = append(adj[e.To], adjacencyEntry{NodeID: e.From, Relation: e.Relation})
	}
	return adj
}

// Tokenize splits s into lowercase keyword tokens (3+ chars).
// Exported for use in relevance filtering outside the knowledge package.
func Tokenize(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) >= 3 {
			tokens[word] = true
		}
	}
	return tokens
}

// tokenize is the unexported alias kept for internal callers.
func tokenize(s string) map[string]bool { return Tokenize(s) }

// KeywordRelevance scores how well text matches keywords (0.0–1.0).
// It tokenizes text and returns the fraction of keywords found.
func KeywordRelevance(text string, keywords map[string]bool) float64 {
	if len(keywords) == 0 {
		return 0
	}
	textTokens := Tokenize(text)
	var overlap int
	for kw := range keywords {
		if textTokens[kw] {
			overlap++
		}
	}
	return float64(overlap) / float64(len(keywords))
}

// computeScore returns a relevance score: 1/depth + keyword overlap + confidence bonus.
func computeScore(node *Node, depth int, keywords map[string]bool) float64 {
	depthScore := 1.0 / float64(depth)
	confidenceBonus := node.Confidence * 0.5

	if len(keywords) == 0 {
		return depthScore + confidenceBonus
	}

	return depthScore + keywordOverlap(node, keywords) + confidenceBonus
}

// keywordScore returns a keyword-only relevance score (no depth or confidence).
func keywordScore(node *Node, keywords map[string]bool) float64 {
	if len(keywords) == 0 {
		return 0
	}
	return keywordOverlap(node, keywords)
}

// keywordOverlap returns the fraction of task keywords found in the node's title, summary, and tags.
func keywordOverlap(node *Node, keywords map[string]bool) float64 {
	if len(keywords) == 0 {
		return 0
	}
	nodeTokens := tokenize(node.Title + " " + node.Summary)
	for _, tag := range node.Tags {
		for tok := range tokenize(tag) {
			nodeTokens[tok] = true
		}
	}
	var overlap int
	for kw := range keywords {
		if nodeTokens[kw] {
			overlap++
		}
	}
	return float64(overlap) / float64(len(keywords))
}

// collectRelations returns distinct edge relation types connecting nodeID back toward seeds/visited.
func collectRelations(edges []*Edge, seeds map[string]bool, visited map[string]bool, nodeID string) []string {
	seen := make(map[string]bool)
	var relations []string
	for _, e := range edges {
		var rel string
		switch {
		case e.From == nodeID && (seeds[e.To] || visited[e.To]):
			rel = e.Relation
		case e.To == nodeID && (seeds[e.From] || visited[e.From]):
			rel = e.Relation
		}
		if rel != "" && !seen[rel] {
			seen[rel] = true
			relations = append(relations, rel)
		}
	}
	return relations
}
