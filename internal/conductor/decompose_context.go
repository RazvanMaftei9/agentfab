package conductor

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/knowledge"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

const (
	decomposeLookupMaxDepth   = 1
	decomposeLookupMaxNodes   = 8
	decomposeLineSummaryChars = 180
	decomposeLineTitleChars   = 72
	decomposeContextMaxChars  = 2600
	decomposeArtifactMaxChars = 1200
)

var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

// appendTruncated appends line to b if budget remains; returns false when full.
func appendTruncated(b *strings.Builder, line string, maxChars int) bool {
	remaining := maxChars - b.Len()
	if remaining <= 0 {
		return false
	}
	if len(line)+1 > remaining {
		if remaining <= 16 {
			return false
		}
		b.WriteString(line[:remaining-16] + " ...[truncated]")
		b.WriteByte('\n')
		return false
	}
	b.WriteString(line)
	b.WriteByte('\n')
	return true
}

func decomposeKnowledge(g *knowledge.Graph, userRequest string) string {
	if g == nil || len(g.Nodes) == 0 {
		return ""
	}

	query := strings.TrimSpace(userRequest)
	if query == "" {
		return ""
	}

	result := knowledge.Lookup(g, "", query, knowledge.LookupOpts{
		MaxDepth: decomposeLookupMaxDepth,
		MaxNodes: decomposeLookupMaxNodes,
	})
	if len(result.Related) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Prior relevant knowledge (shallow lookup):\n")
	for _, rn := range result.Related {
		title := sanitizeKnowledgeLine(rn.Title, decomposeLineTitleChars)
		if title == "" {
			title = sanitizeKnowledgeLine(rn.ID, decomposeLineTitleChars)
		}
		summary := sanitizeKnowledgeLine(rn.Summary, decomposeLineSummaryChars)

		line := fmt.Sprintf("- [%s] %s", rn.Agent, title)
		if req := sanitizeKnowledgeLine(rn.RequestName, 40); req != "" {
			line += fmt.Sprintf(" (from %q)", req)
		}
		if summary != "" {
			line += "\n  " + summary
		}

		if !appendTruncated(&b, line, decomposeContextMaxChars) {
			break
		}
	}

	return strings.TrimSpace(b.String())
}

func decomposeArtifacts(storage runtime.Storage, userRequest string) string {
	ctx := context.Background()

	pattern := "artifacts/"
	var allFiles []string
	for depth := 1; depth <= 5; depth++ {
		p := pattern + strings.Repeat("*/", depth-1) + "*"
		files, err := storage.List(ctx, runtime.TierShared, p)
		if err != nil {
			break
		}
		allFiles = append(allFiles, files...)
	}

	seen := make(map[string]bool, len(allFiles))
	agentFiles := make(map[string][]string)
	for _, f := range allFiles {
		if seen[f] {
			continue
		}
		seen[f] = true
		if isArtifactNoise(f) {
			continue
		}

		parts := strings.SplitN(f, "/", 3)
		if len(parts) < 3 {
			continue
		}
		agent := parts[1]
		rel := parts[2]
		agentFiles[agent] = append(agentFiles[agent], rel)
	}

	if len(agentFiles) == 0 {
		return ""
	}

	// Score each agent's artifacts by keyword relevance to the user request.
	// Always include agents with at least some relevance; if none match,
	// fall back to showing all (the request may reference artifacts implicitly).
	keywords := knowledge.Tokenize(userRequest)
	type agentScore struct {
		name  string
		score float64
	}
	scored := make([]agentScore, 0, len(agentFiles))
	for agent, files := range agentFiles {
		text := agent + " " + strings.Join(files, " ")
		s := knowledge.KeywordRelevance(text, keywords)
		scored = append(scored, agentScore{agent, s})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })

	// If any agent scores above threshold, only include relevant ones.
	const relevanceThreshold = 0.1
	hasRelevant := len(keywords) > 0 && scored[0].score >= relevanceThreshold
	var agents []string
	if hasRelevant {
		for _, as := range scored {
			if as.score >= relevanceThreshold {
				agents = append(agents, as.name)
			}
		}
	} else {
		for _, as := range scored {
			agents = append(agents, as.name)
		}
	}
	sort.Strings(agents)

	var b strings.Builder
	b.WriteString("Existing artifacts on disk:\n")
	for _, agent := range agents {
		files := agentFiles[agent]
		sort.Strings(files)
		line := fmt.Sprintf("- %s: %s", agent, strings.Join(files, ", "))

		if !appendTruncated(&b, line, decomposeArtifactMaxChars) {
			break
		}
	}

	return strings.TrimSpace(b.String())
}

const decomposeDecisionMaxChars = 800

// decomposeDecisions returns active decision nodes relevant to the user request.
// Decisions with no keyword overlap are omitted to avoid injecting noise.
// Conflicts are always included regardless of relevance.
func decomposeDecisions(g *knowledge.Graph, userRequest string) string {
	if g == nil || len(g.Nodes) == 0 {
		return ""
	}

	decisions := g.ActiveDecisions()
	if len(decisions) == 0 {
		return ""
	}

	keywords := knowledge.Tokenize(userRequest)

	// Score and filter decisions by relevance.
	type scored struct {
		node  *knowledge.Node
		score float64
	}
	var relevant []scored
	for _, n := range decisions {
		text := n.Title + " " + n.Summary + " " + strings.Join(n.Tags, " ")
		s := knowledge.KeywordRelevance(text, keywords)
		if len(keywords) == 0 || s >= 0.1 {
			relevant = append(relevant, scored{n, s})
		}
	}

	if len(relevant) == 0 && len(g.DetectDecisionConflicts()) == 0 {
		return ""
	}

	// Sort by score descending.
	sort.Slice(relevant, func(i, j int) bool { return relevant[i].score > relevant[j].score })

	var b strings.Builder
	if len(relevant) > 0 {
		b.WriteString("Active project decisions:\n")
		for _, r := range relevant {
			summary := sanitizeKnowledgeLine(r.node.Summary, decomposeLineSummaryChars)
			if summary == "" {
				summary = sanitizeKnowledgeLine(r.node.Title, decomposeLineSummaryChars)
			}
			line := fmt.Sprintf("- [%s] %s", r.node.Agent, summary)

			if !appendTruncated(&b, line, decomposeDecisionMaxChars) {
				break
			}
		}
	}

	conflicts := g.DetectDecisionConflicts()
	for _, c := range conflicts {
		warning := fmt.Sprintf("CONFLICT [%s]: %s and %s both govern %q — resolve before proceeding",
			c.Tag, c.NodeA, c.NodeB, c.Tag)
		remaining := decomposeDecisionMaxChars - b.Len()
		if remaining <= len(warning)+1 {
			break
		}
		b.WriteString(warning)
		b.WriteByte('\n')
	}

	return strings.TrimSpace(b.String())
}

const decomposeUserRequestMaxChars = 800

func decomposeUserRequests(g *knowledge.Graph, userRequest string) string {
	if g == nil || len(g.Nodes) == 0 {
		return ""
	}

	requests := g.RecentUserRequests(10)
	if len(requests) == 0 {
		return ""
	}

	keywords := knowledge.Tokenize(userRequest)

	// Always include the most recent request for continuity context.
	// For the rest, filter by keyword relevance.
	included := make(map[string]bool)
	var filtered []*knowledge.Node

	if len(requests) > 0 {
		filtered = append(filtered, requests[0])
		included[requests[0].ID] = true
	}

	for _, n := range requests[1:] {
		text := n.Title + " " + n.Summary
		if len(keywords) == 0 || knowledge.KeywordRelevance(text, keywords) >= 0.1 {
			if !included[n.ID] {
				filtered = append(filtered, n)
				included[n.ID] = true
			}
		}
	}

	if len(filtered) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Recent user requests:\n")
	for _, n := range filtered {
		summary := sanitizeKnowledgeLine(n.Summary, decomposeLineSummaryChars)
		if summary == "" {
			summary = sanitizeKnowledgeLine(n.Title, decomposeLineSummaryChars)
		}
		line := fmt.Sprintf("- %s", summary)

		if !appendTruncated(&b, line, decomposeUserRequestMaxChars) {
			break
		}
	}

	return strings.TrimSpace(b.String())
}

func sanitizeKnowledgeLine(s string, max int) string {
	if max <= 0 {
		return ""
	}
	s = htmlTagRE.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:max]) + "..."
}
