package knowledge

import "sort"

// Prune removes stale, low-confidence, and excess nodes. Returns evicted IDs.
func (g *Graph) Prune(opts PruneOpts) []string {
	floor := opts.confidenceFloor()
	maxN := opts.maxNodes()

	var evicted []string

	for id, n := range g.Nodes {
		if n.IsStale() {
			evicted = append(evicted, id)
			delete(g.Nodes, id)
		}
	}

	// Confidence == 0 means "unset" and is NOT evicted.
	for id, n := range g.Nodes {
		if n.Confidence > 0 && n.Confidence < floor {
			evicted = append(evicted, id)
			delete(g.Nodes, id)
		}
	}

	if len(g.Nodes) > maxN {
		type ranked struct {
			id         string
			confidence float64
		}
		var all []ranked
		for id, n := range g.Nodes {
			all = append(all, ranked{id: id, confidence: n.Confidence})
		}
		sort.Slice(all, func(i, j int) bool {
			return all[i].confidence < all[j].confidence
		})

		excess := len(g.Nodes) - maxN
		for i := 0; i < excess; i++ {
			evicted = append(evicted, all[i].id)
			delete(g.Nodes, all[i].id)
		}
	}

	if len(evicted) > 0 {
		clean := make([]*Edge, 0, len(g.Edges))
		for _, e := range g.Edges {
			if g.Nodes[e.From] != nil && g.Nodes[e.To] != nil {
				clean = append(clean, e)
			}
		}
		g.Edges = clean
	}

	return evicted
}
