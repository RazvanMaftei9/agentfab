package ui

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/razvanmaftei/agentfab/internal/event"
)

const maxKnowledgeTreeLines = 10
const maxKnowledgeLabelRunes = 56

// knowledgeTree is a tree representation of knowledge nodes for CLI visualization.
type knowledgeTree struct {
	root     *ktNode
	allNodes []*ktNode
}

// ktNode is a node in the knowledge tree.
type ktNode struct {
	id       string
	agent    string
	title    string
	depth    int
	children []*ktNode
	isOwn    bool
}

// buildKnowledgeTree converts event payload into a tree rooted at a virtual root node.
// Own nodes become direct children of root. Related nodes attach to their edge-connected
// parent among already-placed nodes (BFS order by depth). Returns nil if no nodes.
func buildKnowledgeTree(own []event.KnowledgeNodeInfo, related []event.KnowledgeRelInfo, edges []event.KnowledgeEdgeInfo) *knowledgeTree {
	if len(own) == 0 && len(related) == 0 {
		return nil
	}

	root := &ktNode{id: "root", title: "◆"}
	allNodes := []*ktNode{root}
	placed := make(map[string]*ktNode)

	// Build adjacency from edges (parent → child lookup).
	// An edge From→To means From is a parent of To.
	childToParents := make(map[string][]string)
	for _, e := range edges {
		childToParents[e.To] = append(childToParents[e.To], e.From)
	}

	// Place own nodes as direct children of root.
	for _, n := range own {
		kn := &ktNode{id: n.ID, agent: n.Agent, title: n.Title, depth: 0, isOwn: true}
		root.children = append(root.children, kn)
		placed[n.ID] = kn
		allNodes = append(allNodes, kn)
	}

	// Place related nodes by depth (BFS order), attaching to edge-connected parent.
	// Sort related by depth ascending for correct parent resolution.
	sorted := make([]event.KnowledgeRelInfo, len(related))
	copy(sorted, related)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Depth < sorted[j].Depth })

	for _, rn := range sorted {
		if _, exists := placed[rn.ID]; exists {
			continue // dedup
		}
		kn := &ktNode{id: rn.ID, agent: rn.Agent, title: rn.Title, depth: rn.Depth, isOwn: false}

		// Find a parent among already-placed nodes via edges.
		parentNode := findParent(rn.ID, childToParents, placed)
		if parentNode == nil {
			parentNode = root // fallback to root
		}
		parentNode.children = append(parentNode.children, kn)
		placed[rn.ID] = kn
		allNodes = append(allNodes, kn)
	}

	sortKnowledgeChildren(root)
	return &knowledgeTree{root: root, allNodes: allNodes}
}

// sortKnowledgeChildren ensures deterministic rendering order.
func sortKnowledgeChildren(n *ktNode) {
	if n == nil || len(n.children) == 0 {
		return
	}
	sort.SliceStable(n.children, func(i, j int) bool {
		a := n.children[i]
		b := n.children[j]
		if a.isOwn != b.isOwn {
			return a.isOwn
		}
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		la := strings.ToLower(nodeLabel(a.id, a.title))
		lb := strings.ToLower(nodeLabel(b.id, b.title))
		if la != lb {
			return la < lb
		}
		return a.id < b.id
	})
	for _, c := range n.children {
		sortKnowledgeChildren(c)
	}
}

// findParent looks up edge-connected parents for nodeID among already-placed nodes.
func findParent(nodeID string, childToParents map[string][]string, placed map[string]*ktNode) *ktNode {
	parents := childToParents[nodeID]
	for _, pid := range parents {
		if p, ok := placed[pid]; ok {
			return p
		}
	}
	return nil
}

// nodeLabel returns the display label for a knowledge node.
// Strips the "{agent}/" prefix from the ID if present, prefers title.
func nodeLabel(id, title string) string {
	if title != "" {
		return title
	}
	// Strip agent prefix: "agent/slug" → "slug"
	if idx := strings.IndexByte(id, '/'); idx >= 0 {
		return id[idx+1:]
	}
	return id
}

// renderKnowledgeTreeLines produces terminal lines for the knowledge tree.
// Responsive to width: full → truncated → miniature → nil (hidden).
func renderKnowledgeTreeLines(tree *knowledgeTree, agent string, frame int, width int, tty bool, colorFn func(string) string) []string {
	if tree == nil || tree.root == nil || len(tree.root.children) == 0 {
		return nil
	}

	if width < 15 {
		return nil // hidden tier
	}

	var lines []string
	if width < 26 {
		lines = renderMiniature(tree, agent, frame, tty, colorFn)
		return normalizeKnowledgeTreeLines(lines)
	}

	if width < 44 {
		if candidate := renderTruncated(tree, agent, frame, width, tty, colorFn); len(candidate) > 0 {
			lines = candidate
			return normalizeKnowledgeTreeLines(lines)
		}
		lines = renderMiniature(tree, agent, frame, tty, colorFn)
		return normalizeKnowledgeTreeLines(lines)
	}

	if candidate := renderFullTree(tree, agent, frame, width, tty, colorFn); len(candidate) > 0 {
		lines = candidate
		return normalizeKnowledgeTreeLines(lines)
	}
	if candidate := renderTruncated(tree, agent, frame, width, tty, colorFn); len(candidate) > 0 {
		lines = candidate
		return normalizeKnowledgeTreeLines(lines)
	}
	lines = renderMiniature(tree, agent, frame, tty, colorFn)
	return normalizeKnowledgeTreeLines(lines)
}

// renderFullTree renders a vertical arborescent view with stable branch connectors.
func renderFullTree(tree *knowledgeTree, agent string, frame int, width int, tty bool, colorFn func(string) string) []string {
	maxContentWidth := width - 4 // account for block indent
	if maxContentWidth < 16 {
		return nil
	}

	rootColor := ""
	if tty && colorFn != nil {
		rootColor = colorFn(agent)
	}

	// Compact one-liner for a single node.
	if len(tree.root.children) == 1 && len(tree.root.children[0].children) == 0 {
		child := tree.root.children[0]
		nodeColor := flashColor(frame, tty, colorFn, agent)
		marker := nodeMarker(child)
		prefixVisible := utf8.RuneCountInString("◆──" + marker + " ")
		label := truncateLabel(knowledgeNodeDisplay(child, agent), maxContentWidth-prefixVisible)
		line := "    " +
			colorWrap("◆", rootColor, tty) +
			colorWrap("──", Gray, tty) +
			colorWrap(marker, nodeColor, tty) +
			" " +
			colorWrap(label, nodeColor, tty)
		return []string{line}
	}

	lines := []string{
		"    " + colorWrap("◆", rootColor, tty),
	}
	for i, child := range tree.root.children {
		renderKnowledgeNodeLines(child, "", i == len(tree.root.children)-1, &lines, maxContentWidth, agent, frame, tty, colorFn)
	}
	return clampKnowledgeTreeLines(lines, maxKnowledgeTreeLines, tty)
}

func renderKnowledgeNodeLines(n *ktNode, prefix string, isLast bool, lines *[]string, maxContentWidth int, agent string, frame int, tty bool, colorFn func(string) string) {
	connector := "├──"
	nextPrefix := prefix + "│   "
	if isLast {
		connector = "└──"
		nextPrefix = prefix + "    "
	}

	marker := nodeMarker(n)
	nodeColor := flashColor(frame, tty, colorFn, agent)
	label := knowledgeNodeDisplay(n, agent)

	// Visible width ignores ANSI color codes.
	prefixVisible := utf8.RuneCountInString(prefix+connector) + 1 + utf8.RuneCountInString(marker) + 1
	label = truncateLabel(label, maxContentWidth-prefixVisible)

	line := "    " +
		colorizeTreePrefix(prefix, tty) +
		colorWrap(connector, Gray, tty) +
		" " +
		colorWrap(marker, nodeColor, tty) +
		" " +
		colorWrap(label, nodeColor, tty)
	*lines = append(*lines, line)

	for i, child := range n.children {
		renderKnowledgeNodeLines(child, nextPrefix, i == len(n.children)-1, lines, maxContentWidth, agent, frame, tty, colorFn)
	}
}

func knowledgeNodeDisplay(n *ktNode, currentAgent string) string {
	label := purposeLabel(nodeLabel(n.id, n.title))
	if !n.isOwn && n.agent != "" && n.agent != currentAgent {
		return fmt.Sprintf("%s [%s]", label, n.agent)
	}
	return label
}

// purposeLabel compacts noisy titles into a short, display-friendly purpose.
func purposeLabel(label string) string {
	label = strings.ReplaceAll(label, "\r", " ")
	label = strings.ReplaceAll(label, "\n", " ")
	label = strings.ReplaceAll(label, "\t", " ")
	label = strings.Join(strings.Fields(label), " ")
	label = strings.TrimSpace(label)
	if label == "" {
		return label
	}

	// Cut obvious QA spillovers from upstream context.
	lower := strings.ToLower(label)
	cut := -1
	for _, sep := range []string{" q:", " a:", " question:", " answer:", "\nq:", "\na:", " q ", " a "} {
		if i := strings.Index(lower, sep); i > 0 && (cut == -1 || i < cut) {
			cut = i
		}
	}
	if i := strings.Index(lower, "q:"); i >= 18 && (cut == -1 || i < cut) {
		cut = i
	}
	if i := strings.Index(lower, "a:"); i >= 18 && (cut == -1 || i < cut) {
		cut = i
	}
	if cut > 0 {
		label = strings.TrimSpace(label[:cut])
	}

	// Prefer first sentence for readability in tree view.
	for _, sep := range []string{". ", "! ", "? "} {
		if i := strings.Index(label, sep); i >= 20 {
			label = strings.TrimSpace(label[:i+1])
			break
		}
	}

	if utf8.RuneCountInString(label) > maxKnowledgeLabelRunes {
		label = truncateLabel(label, maxKnowledgeLabelRunes)
	}
	return strings.TrimSpace(label)
}

// normalizeKnowledgeTreeLines removes accidental duplicate root rows and
// sanitizes legacy root labels into a single canonical root line.
func normalizeKnowledgeTreeLines(lines []string) []string {
	if len(lines) == 0 {
		return lines
	}
	out := make([]string, 0, len(lines))
	lastWasRoot := false

	for _, line := range lines {
		plain := strings.TrimSpace(ansiRegex.ReplaceAllString(line, ""))
		if plain == "◆ knowledge" {
			line = "    ◆"
			plain = "◆"
		}
		isRoot := plain == "◆"
		if isRoot && lastWasRoot {
			continue
		}
		out = append(out, line)
		lastWasRoot = isRoot
	}
	return out
}

func nodeMarker(n *ktNode) string {
	if n.isOwn {
		return "●"
	}
	return "○"
}

func colorizeTreePrefix(prefix string, tty bool) string {
	if !tty || prefix == "" {
		return prefix
	}
	var b strings.Builder
	for _, r := range prefix {
		if r == '│' {
			b.WriteString(Gray)
			b.WriteRune(r)
			b.WriteString(Reset)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func clampKnowledgeTreeLines(lines []string, maxLines int, tty bool) []string {
	if len(lines) <= maxLines || maxLines < 2 {
		return lines
	}
	hidden := len(lines) - maxLines + 1
	trunc := fmt.Sprintf("    └── ... +%d more", hidden)
	if tty {
		trunc = "    " + colorWrap("└──", Gray, tty) + " " + colorWrap(fmt.Sprintf("... +%d more", hidden), Gray, tty)
	}
	out := append([]string{}, lines[:maxLines-1]...)
	out = append(out, trunc)
	return out
}

// renderTruncated renders root + leaf labels in a compact form.
func renderTruncated(tree *knowledgeTree, agent string, frame int, width int, tty bool, colorFn func(string) string) []string {
	rootColor := ""
	if tty && colorFn != nil {
		rootColor = colorFn(agent)
	}

	// Collect all leaf labels.
	var labels []string
	collectLeafLabels(tree.root, &labels)
	if len(labels) == 0 {
		return nil
	}

	prefixPlain := "    ◆ → "
	avail := width - utf8.RuneCountInString(prefixPlain)
	if avail < 5 {
		return nil
	}

	var b strings.Builder
	for i, lbl := range labels {
		piece := lbl
		if i > 0 {
			piece = ", " + piece
		}
		if utf8.RuneCountInString(b.String()+piece) > avail {
			if utf8.RuneCountInString(b.String())+3 <= avail {
				b.WriteString("...")
			}
			break
		}
		b.WriteString(piece)
	}

	if b.Len() == 0 {
		b.WriteString(truncateLabel(labels[0], avail))
	}

	return []string{
		"    " +
			colorWrap("◆", rootColor, tty) +
			" " + colorWrap("→", Gray, tty) + " " +
			colorWrap(b.String(), flashColor(frame, tty, colorFn, agent), tty),
	}
}

// collectLeafLabels gathers display labels of all leaf nodes.
func collectLeafLabels(n *ktNode, labels *[]string) {
	if n.id == "root" {
		for _, c := range n.children {
			collectLeafLabels(c, labels)
		}
		return
	}
	if len(n.children) == 0 {
		*labels = append(*labels, nodeLabel(n.id, n.title))
		return
	}
	for _, c := range n.children {
		collectLeafLabels(c, labels)
	}
}

// renderMiniature renders a 2-line placeholder: ◆─┬─ ● / └─ ●
func renderMiniature(tree *knowledgeTree, agent string, frame int, tty bool, colorFn func(string) string) []string {
	rootColor := ""
	dotColor := ""
	if tty && colorFn != nil {
		rootColor = colorFn(agent)
		dotColor = flashColor(frame, tty, colorFn, agent)
	}
	count := len(tree.root.children)
	if count == 0 {
		return nil
	}
	if count == 1 {
		return []string{
			fmt.Sprintf("    %s%s %s",
				colorWrap("◆", rootColor, tty),
				colorWrap("──", Gray, tty),
				colorWrap("●", dotColor, tty)),
		}
	}
	return []string{
		fmt.Sprintf("    %s%s %s",
			colorWrap("◆", rootColor, tty),
			colorWrap("─┬─", Gray, tty),
			colorWrap("●", dotColor, tty)),
		fmt.Sprintf("    %s%s %s",
			colorWrap(" ", "", tty),
			colorWrap(" └─", Gray, tty),
			colorWrap("●", dotColor, tty)),
	}
}

// flashColor returns an agent flash color for the current frame.
func flashColor(frame int, tty bool, colorFn func(string) string, agent string) string {
	if !tty || colorFn == nil {
		return ""
	}
	color := colorFn(agent)
	if frame%4 < 2 {
		return color
	}
	return Dim + color
}

// colorWrap wraps text in an ANSI color code if tty and color is non-empty.
func colorWrap(text, color string, tty bool) string {
	if !tty || color == "" {
		return text
	}
	return color + text + Reset
}

// truncateLabel truncates a label to fit in maxWidth runes.
func truncateLabel(label string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if utf8.RuneCountInString(label) <= maxWidth {
		return label
	}
	if maxWidth <= 3 {
		return truncateRunes(label, maxWidth)
	}
	return truncateRunes(label, maxWidth-3) + "..."
}
