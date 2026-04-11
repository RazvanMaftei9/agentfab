package ui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/razvanmaftei/agentfab/internal/event"
)

type dagNode struct {
	task  event.TaskSummary
	layer int
	row   int // vertical position (in "box rows", each box is 3 lines tall + 1 gap)
	boxW  int // box width in characters
}

type dagLayout struct {
	nodes      []dagNode
	layers     [][]int // layer index → node indices
	maxRow     int     // total vertical rows (in box units)
	layerX     []int   // horizontal start position of each layer
	connectorX []int   // horizontal position of each connector column
	totalW     int     // total width needed
}

const (
	boxMinW      = 14 // minimum box width
	boxPadding   = 2  // left+right padding inside box
	connectorW   = 5  // width of connector column between layers
	boxRowHeight = 4  // 3 lines per box + 1 gap line
	graphIndent  = 2  // left indent
)

func computeLayers(tasks []event.TaskSummary) [][]int {
	n := len(tasks)
	if n == 0 {
		return nil
	}

	idx := make(map[string]int, n)
	for i, t := range tasks {
		idx[t.ID] = i
	}

	children := make([][]int, n)
	inDeg := make([]int, n)
	for i, t := range tasks {
		for _, dep := range t.DependsOn {
			if pi, ok := idx[dep]; ok {
				children[pi] = append(children[pi], i)
				inDeg[i]++
			}
		}
	}

	layer := make([]int, n)
	for i := range layer {
		layer[i] = -1
	}

	queue := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if inDeg[i] == 0 {
			layer[i] = 0
			queue = append(queue, i)
		}
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ch := range children[cur] {
			newLayer := layer[cur] + 1
			if newLayer > layer[ch] {
				layer[ch] = newLayer
			}
			inDeg[ch]--
			if inDeg[ch] == 0 {
				queue = append(queue, ch)
			}
		}
	}

	for i := range layer {
		if layer[i] < 0 {
			layer[i] = 0
		}
	}

	maxLayer := 0
	for _, l := range layer {
		if l > maxLayer {
			maxLayer = l
		}
	}

	layers := make([][]int, maxLayer+1)
	for i, l := range layer {
		layers[l] = append(layers[l], i)
	}

	return layers
}

// layoutDAG computes positions for all nodes and connectors. Returns nil if too wide.
func layoutDAG(tasks []event.TaskSummary, termWidth int) *dagLayout {
	layers := computeLayers(tasks)
	if len(layers) == 0 {
		return nil
	}

	n := len(tasks)
	nodes := make([]dagNode, n)
	for i := range tasks {
		nodes[i].task = tasks[i]
	}

	layerBoxW := make([]int, len(layers))
	for li, group := range layers {
		maxW := boxMinW
		for _, ti := range group {
			w := nodeBoxWidth(tasks[ti])
			if w > maxW {
				maxW = w
			}
		}
		layerBoxW[li] = maxW
	}

	for li, group := range layers {
		for row, ti := range group {
			nodes[ti].layer = li
			nodes[ti].row = row
			nodes[ti].boxW = layerBoxW[li]
		}
	}

	layerX := make([]int, len(layers))
	connectorX := make([]int, 0, len(layers)-1)
	x := graphIndent
	for li := range layers {
		layerX[li] = x
		x += layerBoxW[li]
		if li < len(layers)-1 {
			connectorX = append(connectorX, x)
			x += connectorW
		}
	}
	totalW := x

	if totalW > termWidth {
		return nil
	}

	maxRow := 0
	for _, group := range layers {
		if len(group) > maxRow {
			maxRow = len(group)
		}
	}

	return &dagLayout{
		nodes:      nodes,
		layers:     layers,
		maxRow:     maxRow,
		layerX:     layerX,
		connectorX: connectorX,
		totalW:     totalW,
	}
}

func nodeBoxWidth(t event.TaskSummary) int {
	idW := utf8.RuneCountInString(t.ID)
	if t.LoopID != "" {
		idW += 2 // " ↻" suffix
	}
	w := idW
	if aw := utf8.RuneCountInString(taskAgentLabel(t)); aw > w {
		w = aw
	}
	icon, _ := taskIcon(t.Status)
	sl := utf8.RuneCountInString(statusLabel(t.Status))
	statusW := utf8.RuneCountInString(icon) + 1 + sl
	if statusW > w {
		w = statusW
	}
	w += boxPadding + 2 // +2 for border chars
	if w < boxMinW {
		w = boxMinW
	}
	return w
}

func renderDAG(layout *dagLayout, tasks []event.TaskSummary, tokenMap map[string]taskTokens, frame int, tty bool, colorFn func(string) string) []string {
	if layout == nil {
		return nil
	}

	totalLines := layout.maxRow * boxRowHeight
	if totalLines == 0 {
		totalLines = boxRowHeight
	}

	canvas := make([][]rune, totalLines)
	colors := make([][]string, totalLines) // parallel ANSI color per cell
	for i := range canvas {
		canvas[i] = make([]rune, layout.totalW)
		colors[i] = make([]string, layout.totalW)
		for j := range canvas[i] {
			canvas[i][j] = ' '
		}
	}

	for ni := range layout.nodes {
		nd := &layout.nodes[ni]
		t := tasks[ni]
		x := layout.layerX[nd.layer]
		y := nd.row * boxRowHeight

		agentC := ""
		if colorFn != nil {
			agentC = colorFn(t.Agent)
		}
		if t.Status == "failed" {
			agentC = Red
		}
		borderColor := nodeColor(t.Status, frame, tty, agentC)

		drawBox(canvas, colors, x, y, nd.boxW, t, borderColor, agentC, frame, tty)
	}

	for li := 0; li < len(layout.layers)-1; li++ {
		drawConnectors(canvas, colors, layout, tasks, li, tty)
	}

	lines := make([]string, totalLines)
	for i := range canvas {
		lines[i] = canvasLineToString(canvas[i], colors[i], tty)
	}

	return lines
}

func drawBox(canvas [][]rune, colors [][]string, x, y, w int, t event.TaskSummary, borderColor, agentColor string, frame int, tty bool) {
	if y+2 >= len(canvas) {
		return
	}
	innerW := w - 2 // inside the borders

	setRune(canvas, colors, y, x, '╭', borderColor)
	idStr := " " + t.ID
	if t.LoopID != "" {
		idStr += " ↻"
	}
	idStr += " "
	fillH := innerW - utf8.RuneCountInString(idStr)
	if fillH < 0 {
		fillH = 0
	}
	col := x + 1
	for _, r := range idStr {
		setRune(canvas, colors, y, col, r, borderColor)
		col++
	}
	for i := 0; i < fillH; i++ {
		setRune(canvas, colors, y, col, '─', borderColor)
		col++
	}
	setRune(canvas, colors, y, x+w-1, '╮', borderColor)

	// Line 1: agent name.
	setRune(canvas, colors, y+1, x, '│', borderColor)
	agent := padRight(taskAgentLabel(t), innerW)
	col = x + 1
	for _, r := range agent {
		c := agentColor
		if c == "" {
			c = borderColor
		}
		setRune(canvas, colors, y+1, col, r, c)
		col++
	}
	setRune(canvas, colors, y+1, x+w-1, '│', borderColor)

	// Line 2: status icon + label (always use the canonical label;
	// detailed progress is shown in the snippet bar below the graph).
	setRune(canvas, colors, y+2, x, '│', borderColor)
	icon, _ := taskIcon(t.Status)
	sl := statusLabel(t.Status)
	statusStr := padRight(icon+" "+sl, innerW)
	col = x + 1
	for _, r := range statusStr {
		setRune(canvas, colors, y+2, col, r, borderColor)
		col++
	}
	setRune(canvas, colors, y+2, x+w-1, '│', borderColor)

	// Line 3: bottom border.
	if y+3 < len(canvas) {
		setRune(canvas, colors, y+3, x, '╰', borderColor)
		for i := 1; i < w-1; i++ {
			setRune(canvas, colors, y+3, x+i, '─', borderColor)
		}
		setRune(canvas, colors, y+3, x+w-1, '╯', borderColor)
	}
}

func taskAgentLabel(t event.TaskSummary) string {
	if strings.TrimSpace(t.ExecutionNode) == "" {
		return t.Agent
	}
	return t.Agent + "@" + t.ExecutionNode
}

// drawConnectors renders edge lines between layer li and li+1.
func drawConnectors(canvas [][]rune, colors [][]string, layout *dagLayout, tasks []event.TaskSummary, li int, tty bool) {
	if li >= len(layout.connectorX) {
		return
	}
	cx := layout.connectorX[li]

	// Build index lookup.
	idx := make(map[string]int, len(tasks))
	for i, t := range tasks {
		idx[t.ID] = i
	}

	// For each edge from layer li to li+1, compute connector cells.
	type edge struct {
		srcRow, dstRow int
	}
	var edges []edge

	for _, dstIdx := range layout.layers[li+1] {
		dstNode := &layout.nodes[dstIdx]
		for _, dep := range tasks[dstIdx].DependsOn {
			srcIdx, ok := idx[dep]
			if !ok {
				continue
			}
			srcNode := &layout.nodes[srcIdx]
			if srcNode.layer != li {
				continue // only direct layer-to-layer edges
			}
			edges = append(edges, edge{srcRow: srcNode.row, dstRow: dstNode.row})
		}
	}

	if len(edges) == 0 {
		return
	}

	// Allocate direction bitmask grid for the connector column.
	// Each row in the connector corresponds to a box-row's middle line (line 1).
	dirGrid := make([]uint8, layout.maxRow)

	for _, e := range edges {
		srcLine := e.srcRow
		dstLine := e.dstRow

		// Mark source with RIGHT.
		dirGrid[srcLine] |= DirRight
		// Mark destination with LEFT.
		dirGrid[dstLine] |= DirLeft

		// Mark vertical segments between them.
		minR, maxR := srcLine, dstLine
		if minR > maxR {
			minR, maxR = maxR, minR
		}
		for r := minR; r <= maxR; r++ {
			if r > minR {
				dirGrid[r] |= DirUp
			}
			if r < maxR {
				dirGrid[r] |= DirDown
			}
		}
	}

	// Render the connector column onto the canvas.
	connColor := ""
	if tty {
		connColor = Gray
	}

	for row := 0; row < layout.maxRow; row++ {
		d := dirGrid[row]
		if d == 0 {
			continue
		}

		// The connector attaches to line 1 (middle) of the box at this row.
		lineY := row*boxRowHeight + 1

		ch := boxChar(d)

		// Place the connector character at the center of the connector column.
		midX := cx + connectorW/2
		setRuneStr(canvas, colors, lineY, midX, ch, connColor)

		// Draw horizontal segments from source box right edge to connector.
		if d&DirRight != 0 || d&DirLeft != 0 {
			// From left layer's right edge to connector.
			if d&DirRight != 0 {
				srcBoxEnd := layout.layerX[li] + layout.nodes[layout.layers[li][0]].boxW
				for xx := srcBoxEnd; xx < midX; xx++ {
					setRune(canvas, colors, lineY, xx, '─', connColor)
				}
			}
			// From connector to right layer's left edge.
			if d&DirLeft != 0 {
				dstBoxStart := layout.layerX[li+1]
				for xx := midX + 1; xx < dstBoxStart; xx++ {
					if xx == dstBoxStart-1 {
						setRuneStr(canvas, colors, lineY, xx, ConnArrow, connColor)
					} else {
						setRune(canvas, colors, lineY, xx, '─', connColor)
					}
				}
			}
		}

		// Draw vertical segments.
		if d&DirUp != 0 && row > 0 {
			prevY := (row-1)*boxRowHeight + 1
			for yy := prevY + 1; yy < lineY; yy++ {
				if canvas[yy][midX] == ' ' {
					setRune(canvas, colors, yy, midX, '│', connColor)
				}
			}
		}
	}
}

// boxChar maps a direction bitmask to a box-drawing character.
func boxChar(d uint8) string {
	switch d {
	case DirLeft | DirRight:
		return "─"
	case DirUp | DirDown:
		return "│"
	case DirRight | DirDown:
		return ConnTopLeft
	case DirLeft | DirDown:
		return ConnTopRight
	case DirRight | DirUp:
		return ConnBotLeft
	case DirLeft | DirUp:
		return ConnBotRight
	case DirRight | DirUp | DirDown:
		return ConnTeeRight
	case DirLeft | DirUp | DirDown:
		return ConnTeeLeft
	case DirLeft | DirRight | DirDown:
		return ConnTeeDown
	case DirLeft | DirRight | DirUp:
		return ConnTeeUp
	case DirLeft | DirRight | DirUp | DirDown:
		return ConnCross
	case DirRight:
		return "─"
	case DirLeft:
		return ConnArrow
	case DirUp:
		return "│"
	case DirDown:
		return "│"
	default:
		return " "
	}
}

// nodeColor returns the border color string for a task based on its status.
func nodeColor(status string, frame int, tty bool, agentColor string) string {
	if !tty {
		return ""
	}
	switch status {
	case "completed":
		return agentColor
	case "running":
		if frame%4 < 2 {
			return agentColor
		}
		return Dim + agentColor
	case "failed":
		return Red
	case "cancelled":
		return Gray
	default:
		return Gray
	}
}

// renderSnippetBar produces lines showing streaming snippets for running tasks.
func renderSnippetBar(tasks []event.TaskSummary, progressMap map[string]string, frame int, termWidth int, tty bool, colorFn func(string) string) []string {
	if !tty {
		return nil
	}
	var lines []string
	for _, t := range tasks {
		if t.Status != "running" {
			continue
		}
		snippet, ok := progressMap[t.Agent]
		if !ok || snippet == "" {
			continue
		}
		f := string(frames[frame%len(frames)])
		agentC := ""
		if colorFn != nil {
			agentC = colorFn(t.Agent)
		}
		prefix := fmt.Sprintf("  %s%s%s %s%s%s  ", Cyan, f, Reset, agentC, t.Agent, Reset)
		prefixLen := 2 + 1 + 1 + len(t.Agent) + 2 // "  " + frame + " " + agent + "  "
		avail := termWidth - prefixLen - 4        // quotes + safety
		if avail < 10 {
			continue
		}
		cleaned := tailSnippet(snippet, avail)
		lines = append(lines, fmt.Sprintf("%s%s\"%s\"%s", prefix, Gray, cleaned, Reset))
	}
	return lines
}

// setRune places a rune on the canvas with an associated color.
func setRune(canvas [][]rune, colors [][]string, y, x int, r rune, color string) {
	if y >= 0 && y < len(canvas) && x >= 0 && x < len(canvas[y]) {
		canvas[y][x] = r
		colors[y][x] = color
	}
}

// setRuneStr places the first rune of a string on the canvas (for multi-byte chars).
func setRuneStr(canvas [][]rune, colors [][]string, y, x int, s string, color string) {
	for _, r := range s {
		setRune(canvas, colors, y, x, r, color)
		return // only first rune
	}
}

// canvasLineToString converts a line of the canvas to an ANSI-colored string.
func canvasLineToString(line []rune, lineColors []string, tty bool) string {
	// Find last non-space character to avoid trailing whitespace.
	end := len(line)
	for end > 0 && line[end-1] == ' ' && lineColors[end-1] == "" {
		end--
	}

	if !tty {
		return string(line[:end])
	}

	var b strings.Builder
	curColor := ""
	for i := 0; i < end; i++ {
		c := lineColors[i]
		if c != curColor {
			if curColor != "" {
				b.WriteString(Reset)
			}
			if c != "" {
				b.WriteString(c)
			}
			curColor = c
		}
		b.WriteRune(line[i])
	}
	if curColor != "" {
		b.WriteString(Reset)
	}
	return b.String()
}

// padRight pads s with spaces to at least width w (measured in runes, not bytes).
func padRight(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		// Truncate to w runes.
		i := 0
		for j := 0; j < w; j++ {
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
		}
		return s[:i]
	}
	return s + strings.Repeat(" ", w-n)
}
