package knowledge

// MeasureContextSize estimates the token count injected into an agent's context from knowledge lookup.
func MeasureContextSize(agentGraph, sharedGraph *Graph, agent, taskDesc string) int {
	result := LookupDual(agentGraph, sharedGraph, agent, taskDesc, LookupOpts{})
	chars := 0
	for _, n := range result.Own {
		chars += len(n.Summary) + len(n.Title)
	}
	for _, rn := range result.Related {
		chars += len(rn.Summary) + len(rn.Title)
	}
	return chars / 4 // approximate token count
}

// GraphStats returns basic statistics about a knowledge graph.
func GraphStats(g *Graph) (nodes, edges, decisions int) {
	if g == nil {
		return 0, 0, 0
	}
	nodes = len(g.Nodes)
	edges = len(g.Edges)
	for _, n := range g.Nodes {
		if n.HasTag("decision") {
			decisions++
		}
	}
	return
}
