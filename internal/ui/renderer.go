package ui

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/mattn/go-runewidth"
	"github.com/razvanmaftei/agentfab/internal/event"
	"github.com/razvanmaftei/agentfab/internal/version"
)

var agentPalette = []string{
	Blue, Yellow, Teal, Magenta, Cyan, White,
}

// Renderer consumes events from the bus and draws live feedback.
type Renderer struct {
	w           io.Writer
	tty         bool
	width       int // terminal width for snippet truncation
	agentColor  map[string]string
	nextColor   int
	glamour     *glamour.TermRenderer
	pauseCh     chan struct{}
	pauseAckCh  chan struct{} // renderer acks after erasing animation
	resumeCh    chan struct{}
	layout      *dagLayout // cached DAG layout for current task set
	lastSummary *event.Event

	// Knowledge tree visualization state.
	knowledgeTree      *knowledgeTree // current tree to display
	knowledgeAgent     string         // agent whose knowledge is displayed
	knowledgeTaskID    string         // task whose knowledge is displayed
	knowledgeSignature string         // last rendered knowledge panel signature
	knowledgeFrame     int            // animation frame counter for flash
	knowledgeFlashTill time.Time      // flash window; tree stays visible after this
}

type StartupSummary struct {
	RuntimeMode         string
	ControlPlaneAddress string
	BootstrapNodeIDs    []string
}

// NewRenderer creates a renderer that writes to w.
func NewRenderer(w io.Writer, tty bool) *Renderer {
	width := 80
	if tty {
		width = TermWidth()
	}
	r := &Renderer{
		w:          w,
		tty:        tty,
		width:      width,
		agentColor: make(map[string]string),
		pauseCh:    make(chan struct{}),
		pauseAckCh: make(chan struct{}),
		resumeCh:   make(chan struct{}),
	}
	if tty {
		r.glamour = newGlamourRenderer(width)
	}
	return r
}

// Returns nil on error so callers fall back to plain text.
func newGlamourRenderer(width int) *glamour.TermRenderer {
	style := styles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(width-4), // account for indentation applied later
	)
	if err != nil {
		return nil
	}
	return r
}

func (r *Renderer) renderMarkdown(text string) string {
	if r.glamour == nil {
		return text
	}
	out, err := r.glamour.Render(text)
	if err != nil {
		return text
	}
	return strings.TrimRight(out, "\n")
}

func indentBlock(text, prefix string) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, line := range lines {
		b.WriteString(prefix)
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 2 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func (r *Renderer) assignColor(agent string) string {
	if !r.tty {
		return ""
	}
	if c, ok := r.agentColor[agent]; ok {
		return c
	}
	c := agentPalette[r.nextColor%len(agentPalette)]
	r.agentColor[agent] = c
	r.nextColor++
	return c
}

func (r *Renderer) drawRule(label string) {
	if r.tty {
		if label != "" {
			prefix := BoxHorizontal + BoxHorizontal + BoxHorizontal + " " + Bold + label + Reset + " "
			prefixVisible := 4 + len(label) + 1 // "─── " + label + " "
			remaining := r.width - 2 - prefixVisible
			if remaining < 1 {
				remaining = 1
			}
			fmt.Fprintf(r.w, "  %s%s%s\n", prefix, Gray, strings.Repeat(BoxHorizontal, remaining)+Reset)
		} else {
			fmt.Fprintf(r.w, "  %s%s%s\n", Gray, strings.Repeat(BoxHorizontal, r.width-2), Reset)
		}
	} else {
		if label != "" {
			fmt.Fprintf(r.w, "--- %s ---\n", label)
		} else {
			fmt.Fprintln(r.w, strings.Repeat("-", 40))
		}
	}
}

func (r *Renderer) drawHeader(fabricName string) {
	if !r.tty {
		fmt.Fprintf(r.w, "agentfab v%s -- %s\n", version.Version, fabricName)
		return
	}

	asciiArt := lowerBlockBanner("agentfab", '█')

	fmt.Fprintln(r.w)
	lines := strings.Split(strings.Trim(asciiArt, "\n"), "\n")
	for _, line := range lines {
		fmt.Fprintf(r.w, "%s%s%s\n", White+Bold, line, Reset)
	}
	fmt.Fprintln(r.w)

	fmt.Fprintf(r.w, "  %s%s%sagentfab v%s   %s%s\n", Gray, strings.Repeat("─", 29), Reset, version.Version, Dim, fabricName)
	fmt.Fprintln(r.w)
}

func lowerBlockBanner(word string, fill rune) string {
	templates := map[rune][]string{
		'a': {
			"     ",
			" ### ",
			"    #",
			" ####",
			"#   #",
			" ####",
			"     ",
		},
		'g': {
			"     ",
			" ### ",
			"#   #",
			"#   #",
			" ####",
			"    #",
			" ### ",
		},
		'e': {
			"     ",
			" ### ",
			"#   #",
			"#####",
			"#    ",
			" ### ",
			"     ",
		},
		'n': {
			"     ",
			"#### ",
			"#   #",
			"#   #",
			"#   #",
			"#   #",
			"     ",
		},
		't': {
			"  #  ",
			" ### ",
			"  #  ",
			"  #  ",
			"  #  ",
			"   ##",
			"     ",
		},
		'f': {
			"  ###",
			"  #  ",
			" ####",
			"  #  ",
			"  #  ",
			"  #  ",
			"     ",
		},
		'b': {
			"#    ",
			"#### ",
			"#   #",
			"#   #",
			"#   #",
			"#### ",
			"     ",
		},
	}

	word = strings.ToLower(word)
	const rows = 7
	lines := make([]string, rows)
	fillS := string(fill)

	for _, ch := range word {
		glyph, ok := templates[ch]
		if !ok {
			glyph = []string{"     ", "     ", "     ", "     ", "     ", "     ", "     "}
		}
		for i := 0; i < rows; i++ {
			if len(lines[i]) > 0 {
				lines[i] += "  "
			}
			lines[i] += strings.ReplaceAll(glyph[i], "#", fillS)
		}
	}

	return strings.Join(lines, "\n")
}

// RenderStartup blocks, consuming events until AllAgentsReady.
// It prints each agent as it becomes ready.
func (r *Renderer) RenderStartup(bus event.Bus, fabricName string, agentCount int, summary StartupSummary) {
	if r.tty {
		fmt.Fprint(r.w, ClearScreen)
		r.drawHeader(fabricName)
		r.drawStartupSummary(summary)
		fmt.Fprintf(r.w, "\n  %sAgents%s\n", Bold, Reset)
	} else {
		fmt.Fprintf(r.w, "agentfab v%s -- %s\n", version.Version, fabricName)
		r.drawStartupSummary(summary)
	}

	readyCount := 0
	var spinner *Spinner
	for e := range bus {
		switch e.Type {
		case event.AgentStarting:
			if spinner != nil {
				spinner.Stop()
			}
			if r.tty {
				spinner = NewSpinner(r.w, true)
				spinner.Start(fmt.Sprintf("Starting %s...", e.AgentName))
			} else {
				fmt.Fprintf(r.w, "... %s (%s)\n", e.AgentName, e.AgentModel)
			}
		case event.AgentReady:
			if spinner != nil {
				spinner.Stop()
				spinner = nil
			}
			readyCount++
			if r.tty {
				color := r.assignColor(e.AgentName)
				fmt.Fprintf(r.w, "  %s●%s %s  %s%s%s\n", color, Reset, e.AgentName, Gray, e.AgentModel, Reset)
			} else {
				fmt.Fprintf(r.w, "OK %s (%s)\n", e.AgentName, e.AgentModel)
			}
		case event.AllAgentsReady:
			if spinner != nil {
				spinner.Stop()
				spinner = nil
			}
			if r.tty {
				fmt.Fprintf(r.w, "\n  %s%d agents ready.%s\n\n", Dim, readyCount, Reset)
				r.drawRule("")
				fmt.Fprintln(r.w)
			} else {
				fmt.Fprintf(r.w, "%d agents ready.\n", readyCount)
			}
			return
		}
	}
}

func (r *Renderer) drawStartupSummary(summary StartupSummary) {
	if summary.RuntimeMode == "" && summary.ControlPlaneAddress == "" && len(summary.BootstrapNodeIDs) == 0 {
		return
	}

	if r.tty {
		fmt.Fprintf(r.w, "  %sRuntime%s\n", Bold, Reset)
		if summary.RuntimeMode != "" {
			fmt.Fprintf(r.w, "    %sMode:%s %s\n", Dim, Reset, summary.RuntimeMode)
		}
		if summary.ControlPlaneAddress != "" {
			fmt.Fprintf(r.w, "    %sControl plane:%s %s\n", Dim, Reset, summary.ControlPlaneAddress)
		}
		if len(summary.BootstrapNodeIDs) > 0 {
			fmt.Fprintf(r.w, "    %sLocal nodes:%s %s\n", Dim, Reset, strings.Join(summary.BootstrapNodeIDs, ", "))
		}
		return
	}

	if summary.RuntimeMode != "" {
		fmt.Fprintf(r.w, "Runtime: %s\n", summary.RuntimeMode)
	}
	if summary.ControlPlaneAddress != "" {
		fmt.Fprintf(r.w, "Control plane: %s\n", summary.ControlPlaneAddress)
	}
	if len(summary.BootstrapNodeIDs) > 0 {
		fmt.Fprintf(r.w, "Local nodes: %s\n", strings.Join(summary.BootstrapNodeIDs, ", "))
	}
}

type taskTokens struct {
	inputTokens  int64
	outputTokens int64
}

// RenderRequest blocks, consuming events until RequestComplete.
// It shows decomposition progress, per-task status with tokens, loop transitions,
// and streaming LLM snippets for running tasks.
func (r *Renderer) RenderRequest(bus event.Bus) {
	var spinner *Spinner
	defer func() {
		if spinner != nil {
			spinner.Stop()
		}
	}()

	r.knowledgeTree = nil
	var tasks []event.TaskSummary
	taskLinesDrawn := 0
	taskBlockLines := 0
	taskPrefixLines := 0
	tokenMap := make(map[string]taskTokens) // taskID → tokens
	progressMap := make(map[string]string)  // agent name → streaming snippet
	summaryMap := make(map[string]string)   // taskID → result summary
	knowledgeShown := make(map[string]bool) // taskID → whether we've already shown knowledge context
	spinnerFrame := 0
	executionHeaderDrawn := false

	agentToTask := make(map[string]string)

	var tickerC <-chan time.Time
	if r.tty {
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		tickerC = ticker.C
	}

	for {
		select {
		case e, ok := <-bus:
			if !ok {
				return
			}
			switch e.Type {
			case event.RequestReceived:
				if spinner == nil {
					spinner = NewSpinner(r.w, r.tty)
					spinner.Start("Processing request...")
				}

			case event.RequestScreened:
				if spinner != nil {
					spinner.Stop()
					spinner = nil
				}
				if r.tty {
					fmt.Fprintf(r.w, "  %s%s%s\n", Dim, e.ScreenMessage, Reset)
				} else {
					fmt.Fprintln(r.w, e.ScreenMessage)
				}

			case event.DecomposeStart:
				if spinner != nil {
					spinner.Stop()
				}
				spinner = NewSpinner(r.w, r.tty)
				spinner.Start("Decomposing request...")

			case event.DecomposeEnd:
				if spinner != nil {
					spinner.Stop()
					spinner = nil
				}
				tasks = e.Tasks
				if len(tasks) > 0 {
					r.drawRule("Decomposition")
					fmt.Fprintln(r.w)
					r.drawTaskPlan(tasks)
					if e.InputTokens > 0 || e.OutputTokens > 0 {
						r.printTokenLine(e.InputTokens, e.OutputTokens, e.TotalCalls)
					}
					fmt.Fprintln(r.w)
				} else if e.InputTokens > 0 || e.OutputTokens > 0 {
					r.printTokenLine(e.InputTokens, e.OutputTokens, e.TotalCalls)
				}

			case event.TaskStart:
				if !executionHeaderDrawn {
					executionHeaderDrawn = true
					r.drawRule("Execution")
					fmt.Fprintln(r.w)
				}

				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				}

				taskPrefixLines = 0
				taskLabel := taskEventLabel(tasks, e.TaskID, e.TaskAgent, e.ExecutionNode)
				if r.tty && e.TaskDescription != "" {
					color := r.assignColor(e.TaskAgent)
					desc := e.TaskDescription
					desc = strings.ReplaceAll(desc, "\n", " ")
					desc = strings.TrimSpace(desc)
					maxDesc := r.width - 8 // 4 indent + margin
					if maxDesc > 10 && utf8.RuneCountInString(desc) > maxDesc {
						desc = truncateRunes(desc, maxDesc-3) + "..."
					}
					fmt.Fprintf(r.w, "  %s\u25b8%s %s%s%s %s%s%s\n",
						Cyan, Reset, Bold, e.TaskID, Reset, color, taskLabel, Reset)
					fmt.Fprintf(r.w, "    %s%s%s\n", Dim, desc, Reset)
					taskPrefixLines = 2
				}
				updateTask(tasks, e.TaskID, "running", e.ExecutionNode)
				agentToTask[e.TaskAgent] = e.TaskID

				if r.tty {
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskLinesDrawn == 0)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				} else {
					taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, 0)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.TaskProgress:
				taskID := e.TaskID
				if taskID == "" {
					taskID = agentToTask[e.TaskAgent]
				}
				if taskID != "" && strings.TrimSpace(e.ExecutionNode) != "" {
					updateTaskExecutionNode(tasks, taskID, e.ExecutionNode)
				}
				if r.tty {
					if e.TaskAgent == "conductor" && spinner != nil {
						snippet := tailSnippet(e.ProgressText, 60)
						spinner.Update("Decomposing...  " + Dim + "\"" + snippet + "\"" + Reset)
					} else {
						progressMap[e.TaskAgent] = e.ProgressText
						if taskLinesDrawn > 0 {
							taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
							taskLinesDrawn = taskBlockLines + taskPrefixLines
						}
					}
				}

			case event.TaskComplete:
				updateTask(tasks, e.TaskID, "completed", e.ExecutionNode)
				taskLabel := taskEventLabel(tasks, e.TaskID, e.TaskAgent, e.ExecutionNode)
				tokenMap[e.TaskID] = taskTokens{
					inputTokens:  e.AgentInputTokens,
					outputTokens: e.AgentOutputTokens,
				}
				if e.ResultSummary != "" {
					summaryMap[e.TaskID] = e.ResultSummary
				}
				delete(progressMap, e.TaskAgent)
				if r.tty {
					if r.layout != nil && taskLinesDrawn > 0 {
						EraseBlock(r.w, taskBlockLines+taskPrefixLines)
						r.printEventLine("✓", r.assignColor(e.TaskAgent), e.TaskID, taskLabel, e.ResultSummary, Dim, tokenMap[e.TaskID])
						taskPrefixLines = 1
						taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
						taskLinesDrawn = taskBlockLines + taskPrefixLines
					} else {
						taskPrefixLines = 0
						taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
						taskLinesDrawn = taskBlockLines + taskPrefixLines
					}
				} else {
					if s, ok := summaryMap[e.TaskID]; ok {
						fmt.Fprintf(r.w, "  > %s\n", s)
					}
					suffix := tokenSuffix(e.TaskID, tokenMap)
					fmt.Fprintf(r.w, "%s %s %s %s%s\n", statusPrefix("completed"), e.TaskID, taskLabel, statusLabel("completed"), suffix)
				}

			case event.TaskFailed:
				updateTask(tasks, e.TaskID, "failed", e.ExecutionNode)
				taskLabel := taskEventLabel(tasks, e.TaskID, e.TaskAgent, e.ExecutionNode)
				tokenMap[e.TaskID] = taskTokens{
					inputTokens:  e.AgentInputTokens,
					outputTokens: e.AgentOutputTokens,
				}
				delete(progressMap, e.TaskAgent)
				if r.tty {
					if r.layout != nil && taskLinesDrawn > 0 {
						EraseBlock(r.w, taskBlockLines+taskPrefixLines)
						r.printEventLine("✗", Red, e.TaskID, taskLabel, e.ErrMsg, Red, tokenMap[e.TaskID])
						taskPrefixLines = 1
						taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
						taskLinesDrawn = taskBlockLines + taskPrefixLines
					} else {
						taskPrefixLines = 0
						taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
						taskLinesDrawn = taskBlockLines + taskPrefixLines
					}
				} else {
					suffix := tokenSuffix(e.TaskID, tokenMap)
					errDetail := ""
					if e.ErrMsg != "" {
						errDetail = " — " + e.ErrMsg
					}
					fmt.Fprintf(r.w, "%s %s %s %s%s%s\n", statusPrefix("failed"), e.TaskID, taskLabel, statusLabel("failed"), errDetail, suffix)
				}

			case event.LoopTransition:
				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
					r.printLoopTransition(e)
					taskPrefixLines = 1
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				} else {
					r.printLoopTransition(e)
				}

			case event.TaskAmended:
				updateTask(tasks, e.AmendedTaskID, "pending", "")
				delete(progressMap, e.AmendedAgent)
				if e.AmendedTaskID != "" {
					delete(knowledgeShown, e.AmendedTaskID)
				}
				if r.tty && taskLinesDrawn > 0 {
					taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.KnowledgeLookup:
				totalNodes := len(e.KnowledgeLookupOwnNodes) + len(e.KnowledgeLookupRelNodes)
				if totalNodes == 0 {
					break
				}
				taskID := strings.TrimSpace(e.KnowledgeLookupTask)
				if taskID != "" && knowledgeShown[taskID] {
					break
				}
				tree := buildKnowledgeTree(e.KnowledgeLookupOwnNodes, e.KnowledgeLookupRelNodes, e.KnowledgeLookupEdges)
				signature := r.knowledgePanelSignature(tree, e.KnowledgeLookupAgent, e.KnowledgeLookupTask)
				if e.KnowledgeLookupTask == r.knowledgeTaskID && signature == r.knowledgeSignature {
					break
				}
				if r.tty {
					hadKnowledgePanel := r.knowledgeTaskID != ""
					r.knowledgeTree = tree
					r.knowledgeAgent = e.KnowledgeLookupAgent
					r.knowledgeTaskID = e.KnowledgeLookupTask
					r.knowledgeSignature = signature
					if taskID != "" {
						knowledgeShown[taskID] = true
					}
					r.knowledgeFlashTill = time.Now().Add(800 * time.Millisecond)
					r.knowledgeFrame = 0
					if taskLinesDrawn == 0 || !hasRunningTasks(tasks) || !hadKnowledgePanel {
						// No active animation loop is going to refresh the panel for us.
						// Redraw immediately in that case.
						if taskLinesDrawn > 0 {
							taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
							taskLinesDrawn = taskBlockLines + taskPrefixLines
						} else if len(tasks) > 0 {
							taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
							taskLinesDrawn = taskBlockLines + taskPrefixLines
						}
					}
				} else {
					fmt.Fprintf(r.w, "  knowledge: %d own + %d related nodes for %s\n",
						len(e.KnowledgeLookupOwnNodes), len(e.KnowledgeLookupRelNodes), e.KnowledgeLookupAgent)
				}

			case event.CurationStarted:
				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				}
				if r.tty {
					color := r.assignColor(e.CurationAgent)
					fmt.Fprintf(r.w, "  %s◐%s %s%s%s: curating knowledge (%d nodes)...\n",
						Cyan, Reset, color, e.CurationAgent, Reset, e.CurationNodesIn)
					taskPrefixLines = 1
				} else {
					fmt.Fprintf(r.w, "CURATE %s: curating knowledge (%d nodes)\n",
						e.CurationAgent, e.CurationNodesIn)
				}
				if r.tty && len(tasks) > 0 {
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.CurationComplete:
				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				}
				if r.tty {
					color := r.assignColor(e.CurationAgent)
					fmt.Fprintf(r.w, "  %s●%s %s%s%s: curation complete (%d → %d nodes",
						Green, Reset, color, e.CurationAgent, Reset,
						e.CurationNodesIn, e.CurationNodesOut)
					if e.ColdStorageMoved > 0 {
						fmt.Fprintf(r.w, ", %d archived", e.ColdStorageMoved)
					}
					if e.ColdStoragePurged > 0 {
						fmt.Fprintf(r.w, ", %d purged", e.ColdStoragePurged)
					}
					fmt.Fprintf(r.w, ")%s\n", Reset)
					taskPrefixLines = 1
				} else {
					fmt.Fprintf(r.w, "CURATE %s: complete (%d -> %d nodes, %d archived, %d purged)\n",
						e.CurationAgent, e.CurationNodesIn, e.CurationNodesOut,
						e.ColdStorageMoved, e.ColdStoragePurged)
				}
				if r.tty && len(tasks) > 0 {
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.AgentSleep:
				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				}
				if r.tty {
					color := r.assignColor(e.AgentName)
					fmt.Fprintf(r.w, "  %s◌%s %s%s%s: entering sleep state\n",
						Gray, Reset, color, e.AgentName, Reset)
					taskPrefixLines = 1
				}
				if r.tty && len(tasks) > 0 {
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.AgentWake:
				if r.tty && taskLinesDrawn > 0 {
					EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				}
				if r.tty {
					color := r.assignColor(e.AgentName)
					fmt.Fprintf(r.w, "  %s●%s %s%s%s: waking from sleep\n",
						Cyan, Reset, color, e.AgentName, Reset)
					taskPrefixLines = 1
				}
				if r.tty && len(tasks) > 0 {
					taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, false)
					taskLinesDrawn = taskBlockLines + taskPrefixLines
				}

			case event.RequestCancelled:
				if r.tty {
					fmt.Fprintf(r.w, "\n  %sRequest cancelled.%s\n\n", Gray, Reset)
				} else {
					fmt.Fprintln(r.w, "Request cancelled.")
				}
				return

			case event.RequestComplete:
				ev := e
				r.lastSummary = &ev
				return
			}

		case <-r.pauseCh:
			var spinnerMsg string
			if spinner != nil {
				spinnerMsg = spinner.Msg()
				spinner.Stop()
				spinner = nil
			}
			r.knowledgeTree = nil
			r.knowledgeTaskID = ""
			r.knowledgeSignature = ""
			if r.tty && taskLinesDrawn > 0 {
				EraseBlock(r.w, taskBlockLines+taskPrefixLines)
				taskBlockLines = 0
				taskPrefixLines = 0
				taskLinesDrawn = 0
			}
			r.pauseAckCh <- struct{}{}
			<-r.resumeCh
			if spinnerMsg != "" {
				spinner = NewSpinner(r.w, r.tty)
				spinner.Start(spinnerMsg)
			}
			if len(tasks) > 0 && executionHeaderDrawn {
				taskBlockLines = r.drawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, true)
				taskLinesDrawn = taskBlockLines + taskPrefixLines
			}

		case <-tickerC:
			if taskLinesDrawn > 0 && hasRunningTasks(tasks) {
				spinnerFrame++
				taskBlockLines = r.redrawTaskBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame, taskBlockLines)
				taskLinesDrawn = taskBlockLines + taskPrefixLines
			}
		}
	}
}

func (r *Renderer) knowledgePanelSignature(tree *knowledgeTree, agent, taskID string) string {
	if tree == nil {
		return taskID + "|" + agent
	}
	width := r.width
	if width <= 0 {
		width = TermWidth()
	}
	plainColorFn := func(string) string { return "" }
	lines := renderKnowledgeTreeLines(tree, agent, 0, width, false, plainColorFn)
	var b strings.Builder
	b.WriteString(taskID)
	b.WriteByte('|')
	b.WriteString(agent)
	b.WriteByte('|')
	b.WriteString(fmt.Sprintf("%d", tree.totalNodes))
	for _, line := range lines {
		b.WriteByte('|')
		b.WriteString(line)
	}
	return b.String()
}

func updateTask(tasks []event.TaskSummary, id, status, executionNode string) {
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].Status = status
			if strings.TrimSpace(executionNode) != "" {
				tasks[i].ExecutionNode = executionNode
			}
			return
		}
	}
}

func updateTaskExecutionNode(tasks []event.TaskSummary, id, executionNode string) {
	if strings.TrimSpace(executionNode) == "" {
		return
	}
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].ExecutionNode = executionNode
			return
		}
	}
}

func taskEventLabel(tasks []event.TaskSummary, taskID, fallbackAgent, fallbackNode string) string {
	for _, task := range tasks {
		if task.ID == taskID {
			return taskAgentLabel(task)
		}
	}
	if strings.TrimSpace(fallbackNode) == "" {
		return fallbackAgent
	}
	return fallbackAgent + "@" + fallbackNode
}

// drawTaskPlan prints the task plan list after decomposition.
// TTY mode attempts a DAG graph; falls back to flat list if terminal is too narrow.
func (r *Renderer) drawTaskPlan(tasks []event.TaskSummary) {
	if r.tty {
		fmt.Fprintf(r.w, "  %s%d tasks planned%s\n", Dim, len(tasks), Reset)
		layout := layoutDAG(tasks, r.width)
		if layout != nil {
			colorFn := func(agent string) string { return r.assignColor(agent) }
			lines := renderDAG(layout, tasks, nil, 0, true, colorFn)
			for _, line := range lines {
				fmt.Fprintln(r.w, line)
			}
		} else {
			for _, t := range tasks {
				color := r.assignColor(t.Agent)
				desc := t.Description
				if desc == "" {
					desc = "-"
				}
				loopTag := ""
				if t.LoopID != "" {
					loopTag = fmt.Sprintf("  %s↻ %s%s", Cyan, t.LoopID, Reset)
				}
				line := fmt.Sprintf("    %s%-4s%s %s%s%s  %s", Bold, t.ID, Reset, color, t.Agent, Reset, desc)
				if len(t.DependsOn) > 0 {
					line += fmt.Sprintf("  %s(depends: %s)%s", Gray, strings.Join(t.DependsOn, ", "), Reset)
				}
				line += loopTag
				fmt.Fprintln(r.w, line)
			}
		}
	} else {
		fmt.Fprintf(r.w, "%d tasks planned\n", len(tasks))
		for _, t := range tasks {
			desc := t.Description
			if desc == "" {
				desc = "-"
			}
			loopTag := ""
			if t.LoopID != "" {
				loopTag = fmt.Sprintf(" [loop: %s]", t.LoopID)
			}
			line := fmt.Sprintf("  %-4s %s  %s", t.ID, t.Agent, desc)
			if len(t.DependsOn) > 0 {
				line += fmt.Sprintf(" (depends: %s)", strings.Join(t.DependsOn, ", "))
			}
			line += loopTag
			fmt.Fprintln(r.w, line)
		}
	}
}

func (r *Renderer) renderAnimatedBlock(tasks []event.TaskSummary, tokenMap map[string]taskTokens, progressMap map[string]string, summaryMap map[string]string, spinnerFrame int) ([]string, int) {
	var b strings.Builder
	oldW := r.w
	r.w = &b
	defer func() { r.w = oldW }()
	pinnedTopLines := 0

	if r.knowledgeTree != nil {
		frame := 0
		if time.Now().Before(r.knowledgeFlashTill) {
			r.knowledgeFrame++
			frame = r.knowledgeFrame
		} else {
			frame = 0
		}
		color := r.assignColor(r.knowledgeAgent)
		fmt.Fprintf(r.w, "  %s◈%s %s%s%s using %d knowledge nodes\n",
			Cyan, Reset, color, r.knowledgeAgent, Reset, r.knowledgeTree.totalNodes)
		colorFn := func(agent string) string { return r.assignColor(agent) }
		treeLines := renderKnowledgeTreeLines(r.knowledgeTree, r.knowledgeAgent, frame, r.width, r.tty, colorFn)
		for _, line := range treeLines {
			fmt.Fprintln(r.w, line)
		}
		if len(treeLines) > 0 {
			fmt.Fprintln(r.w)                   // blank separator
			pinnedTopLines = len(treeLines) + 2 // +2 for header + separator
		} else {
			pinnedTopLines = 1 // just the header
		}
	}

	if r.layout != nil {
		fmt.Fprintln(r.w)
		colorFn := func(agent string) string { return r.assignColor(agent) }
		lines := renderDAG(r.layout, tasks, tokenMap, spinnerFrame, true, colorFn)
		for _, line := range lines {
			fmt.Fprintln(r.w, line)
		}
		snippets := renderSnippetBar(tasks, progressMap, spinnerFrame, r.width, true, colorFn)
		for _, line := range snippets {
			fmt.Fprintln(r.w, line)
		}
	} else {
		for _, t := range tasks {
			if summary, ok := summaryMap[t.ID]; ok {
				r.drawSummaryLine(summary)
			}
			r.drawTaskLineWithProgress(t, tokenMap, progressMap, spinnerFrame)
		}
	}
	r.drawHints()

	s := b.String()
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return nil, 0
	}
	return strings.Split(s, "\n"), pinnedTopLines
}

func (r *Renderer) printClippedBlock(lines []string, pinnedTop int) int {
	if len(lines) == 0 {
		return 0
	}
	termHeight := TermHeight()
	maxLines := termHeight - 2
	if maxLines < 5 {
		maxLines = 5
	}

	if len(lines) > maxLines {
		if pinnedTop > maxLines-2 {
			pinnedTop = maxLines - 2
		}
		if pinnedTop > len(lines) {
			pinnedTop = len(lines)
		}
		if pinnedTop > 0 {
			tailBudget := maxLines - pinnedTop - 1
			if tailBudget < 1 {
				tailBudget = 1
			}
			tailStart := len(lines) - tailBudget
			if tailStart < pinnedTop {
				tailStart = pinnedTop
			}

			clipped := make([]string, 0, pinnedTop+1+(len(lines)-tailStart))
			clipped = append(clipped, lines[:pinnedTop]...)
			clipped = append(clipped, fmt.Sprintf("  %s... tasks truncated to fit terminal ...%s", Dim, Reset))
			clipped = append(clipped, lines[tailStart:]...)
			lines = clipped
		} else {
			lines = lines[len(lines)-maxLines:]
			lines[0] = fmt.Sprintf("  %s... tasks above truncated to fit terminal ...%s", Dim, Reset)
		}
	}

	width := r.width
	if width <= 0 {
		width = TermWidth()
	}

	// Print each line, truncating to terminal width to prevent soft wrapping.
	// Soft-wrapped lines cause EraseBlock's CUU count to be wrong, leaving ghosts.
	rows := 0
	for _, line := range lines {
		vl := visibleLength(line)
		if vl >= width {
			line = truncateVisual(line, width-1)
		}
		fmt.Fprintln(r.w, line)
		rows++
	}
	return rows
}

func (r *Renderer) drawTaskBlock(tasks []event.TaskSummary, tokenMap map[string]taskTokens, progressMap map[string]string, summaryMap map[string]string, spinnerFrame int, initial bool) int {
	if !r.tty {
		drawn := 0
		for _, t := range tasks {
			if summary, ok := summaryMap[t.ID]; ok {
				fmt.Fprintf(r.w, "  > %s\n", summary)
				drawn++
			}
			prefix := statusPrefix(t.Status)
			suffix := tokenSuffix(t.ID, tokenMap)
			fmt.Fprintf(r.w, "%s %s %s %s%s\n", prefix, t.ID, taskAgentLabel(t), statusLabel(t.Status), suffix)
			drawn++
		}
		return drawn
	}

	r.width = TermWidth()
	r.layout = layoutDAG(tasks, r.width)

	lines, pinnedTop := r.renderAnimatedBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame)
	return r.printClippedBlock(lines, pinnedTop)
}

func (r *Renderer) redrawTaskBlock(tasks []event.TaskSummary, tokenMap map[string]taskTokens, progressMap map[string]string, summaryMap map[string]string, spinnerFrame int, lines int) int {
	if !r.tty || lines == 0 {
		return lines
	}
	EraseBlock(r.w, lines)
	r.width = TermWidth()
	r.layout = layoutDAG(tasks, r.width)

	blockLines, pinnedTop := r.renderAnimatedBlock(tasks, tokenMap, progressMap, summaryMap, spinnerFrame)
	return r.printClippedBlock(blockLines, pinnedTop)
}

func (r *Renderer) drawSummaryLine(summary string) {
	summary = strings.ReplaceAll(summary, "\n", " ")
	summary = strings.TrimSpace(summary)
	avail := r.width - 8 // "    → " prefix
	if avail > 10 && utf8.RuneCountInString(summary) > avail {
		summary = truncateRunes(summary, avail-3) + "..."
	}
	fmt.Fprintf(r.w, "    %s→%s %s%s%s\n", Dim, Reset, Gray, summary, Reset)
}

func (r *Renderer) printEventLine(icon, iconColor, taskID, agent, detail, detailColor string, tok taskTokens) {
	color := r.assignColor(agent)
	if icon == "✗" {
		color = iconColor
	}

	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(iconColor)
	b.WriteString(icon)
	b.WriteString(Reset)
	b.WriteString(" ")
	b.WriteString(Bold)
	b.WriteString(taskID)
	b.WriteString(Reset)
	b.WriteString("  ")
	b.WriteString(color)
	b.WriteString(agent)
	b.WriteString(Reset)

	tokStr := ""
	tokVisLen := 0
	if tok.inputTokens > 0 || tok.outputTokens > 0 {
		tokInner := fmt.Sprintf("(%s in / %s out)", formatTokens(tok.inputTokens), formatTokens(tok.outputTokens))
		tokStr = "  " + Gray + tokInner + Reset
		tokVisLen = 2 + len(tokInner)
	}

	if detail != "" {
		detail = strings.ReplaceAll(detail, "\n", " ")
		detail = strings.ReplaceAll(detail, "\r", " ")
		detail = strings.TrimSpace(detail)

		prefixLen := 2 + 1 + 1 + len(taskID) + 2 + utf8.RuneCountInString(agent) + 4
		avail := r.width - prefixLen - tokVisLen - 1
		if avail < 4 {
			detail = "" // no room; omit to prevent line wrapping
		} else if utf8.RuneCountInString(detail) > avail {
			detail = truncateRunes(detail, avail-3) + "..."
		}

		b.WriteString(" ")
		b.WriteString(Gray)
		b.WriteString("──")
		b.WriteString(Reset)
		b.WriteString(" ")
		if detailColor != "" {
			b.WriteString(detailColor)
		}
		b.WriteString(detail)
		if detailColor != "" {
			b.WriteString(Reset)
		}
	}

	if tokStr != "" {
		b.WriteString(tokStr)
	}

	fmt.Fprintln(r.w, b.String())
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	count := 0
	for i < len(s) && count < n {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		count++
	}
	return s[:i]
}

func (r *Renderer) drawHints() {
	fmt.Fprintf(r.w, "  %s%sp%s pause  %sr%s resume  %sc%s cancel  %s!%s chat%s\n",
		Dim, Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim, Reset)
}

func (r *Renderer) drawTaskLineWithProgress(t event.TaskSummary, tokenMap map[string]taskTokens, progressMap map[string]string, spinnerFrame int) {
	tok := tokenSuffix(t.ID, tokenMap)
	agentColor := r.assignColor(t.Agent)
	agentLabel := taskAgentLabel(t)

	if t.Status == "running" {
		frame := string(frames[spinnerFrame%len(frames)])
		baseLine := fmt.Sprintf("  %s %s %s %s%s", frame, t.ID, agentLabel, animatedStatusLabel(t.Status, spinnerFrame), tok)
		baseLen := visibleLength(baseLine)

		snippet, ok := progressMap[t.Agent]
		if ok && snippet != "" {
			avail := r.width - baseLen - 6
			if avail > 10 {
				cleaned := tailSnippet(snippet, avail)
				fmt.Fprintf(r.w, "  %s%s%s %s%s%s %s%s%s %s%s%s  %s\"%s\"%s\n",
					agentColor, frame, Reset,
					Bold, t.ID, Reset,
					agentColor, agentLabel, Reset,
					Dim, animatedStatusLabel(t.Status, spinnerFrame), tok,
					Gray, cleaned, Reset)
				return
			}
		}
		// No snippet or not enough room — render without.
		fmt.Fprintf(r.w, "  %s%s%s %s%s%s %s%s%s %s%s%s%s\n",
			agentColor, frame, Reset,
			Bold, t.ID, Reset,
			agentColor, agentLabel, Reset,
			Dim, animatedStatusLabel(t.Status, spinnerFrame), tok, Reset)
		return
	}

	icon, _ := taskIcon(t.Status)
	iconColor := agentColor
	if t.Status == "failed" {
		iconColor = Red
	} else if t.Status == "cancelled" || t.Status == "pending" {
		iconColor = Gray
	}

	fmt.Fprintf(r.w, "  %s%s%s %s%s%s %s%s%s %s%s%s%s\n",
		iconColor, icon, Reset,
		Bold, t.ID, Reset,
		agentColor, agentLabel, Reset,
		Dim, statusLabel(t.Status), tok, Reset)
}

func (r *Renderer) printTokenLine(inTok, outTok, calls int64) {
	if r.tty {
		fmt.Fprintf(r.w, "  %sTokens: %s in / %s out (%d %s)%s\n",
			Gray, formatTokens(inTok), formatTokens(outTok), calls, plural("call", calls), Reset)
	} else {
		fmt.Fprintf(r.w, "Tokens: %s in / %s out (%d %s)\n",
			formatTokens(inTok), formatTokens(outTok), calls, plural("call", calls))
	}
}

func (r *Renderer) printLoopTransition(e event.Event) {
	if r.tty {
		fmt.Fprintf(r.w, "  %s↻%s %s: %s → %s", Cyan, Reset, e.LoopID, e.FromState, e.ToState)
		if e.Verdict != "" {
			fmt.Fprintf(r.w, " %s(verdict: %s)%s", Gray, e.Verdict, Reset)
		}
		fmt.Fprintln(r.w)
	} else {
		line := fmt.Sprintf("LOOP %s: %s -> %s", e.LoopID, e.FromState, e.ToState)
		if e.Verdict != "" {
			line += fmt.Sprintf(" (verdict: %s)", e.Verdict)
		}
		fmt.Fprintln(r.w, line)
	}
}

func (r *Renderer) printSummary(e event.Event) {
	dur := e.TotalDuration.Round(100 * time.Millisecond)
	totalTok := e.InputTokens + e.OutputTokens

	cacheSuffix := ""
	if e.CacheReadTokens > 0 && e.InputTokens > 0 {
		pct := e.CacheReadTokens * 100 / e.InputTokens
		cacheSuffix = fmt.Sprintf(", %d%% cached", pct)
	}

	if r.tty {
		fmt.Fprintln(r.w)
		if e.HasFailures {
			fmt.Fprintf(r.w, "  %sDone with errors%s (%s)", Red, Reset, dur)
		} else {
			fmt.Fprintf(r.w, "  %sDone%s (%s)", Green, Reset, dur)
		}
		if totalTok > 0 {
			fmt.Fprintf(r.w, " — %s tokens %s(%s in / %s out, %d %s%s)%s",
				formatTokens(totalTok), Gray,
				formatTokens(e.InputTokens), formatTokens(e.OutputTokens),
				e.TotalCalls, plural("call", e.TotalCalls), cacheSuffix, Reset)
		}
		fmt.Fprintln(r.w)
		for _, s := range e.FailureSummaries {
			fmt.Fprintf(r.w, "  %s• %s%s\n", Red, s, Reset)
		}
		if len(e.ModelUsages) > 1 {
			fmt.Fprintf(r.w, "  %sUsage by model:%s\n", Dim, Reset)
			for _, mu := range e.ModelUsages {
				muCache := ""
				if mu.CacheReadTokens > 0 && mu.InputTokens > 0 {
					pct := mu.CacheReadTokens * 100 / mu.InputTokens
					muCache = fmt.Sprintf(", %d%% cached", pct)
				}
				fmt.Fprintf(r.w, "    %s%-35s%s %s in / %s out  %s(%d %s%s)%s\n",
					Gray, mu.Model, Reset,
					formatTokens(mu.InputTokens), formatTokens(mu.OutputTokens),
					Dim, mu.TotalCalls, plural("call", mu.TotalCalls), muCache, Reset)
			}
		}
		fmt.Fprintln(r.w)
	} else {
		if e.HasFailures {
			fmt.Fprintf(r.w, "Done with errors (%s)", dur)
		} else {
			fmt.Fprintf(r.w, "Done (%s)", dur)
		}
		if totalTok > 0 {
			fmt.Fprintf(r.w, " — %s tokens (%s in / %s out, %d %s%s)",
				formatTokens(totalTok),
				formatTokens(e.InputTokens), formatTokens(e.OutputTokens),
				e.TotalCalls, plural("call", e.TotalCalls), cacheSuffix)
		}
		fmt.Fprintln(r.w)
		for _, s := range e.FailureSummaries {
			fmt.Fprintf(r.w, "• %s\n", s)
		}
		if len(e.ModelUsages) > 1 {
			fmt.Fprintln(r.w, "Usage by model:")
			for _, mu := range e.ModelUsages {
				muCache := ""
				if mu.CacheReadTokens > 0 && mu.InputTokens > 0 {
					pct := mu.CacheReadTokens * 100 / mu.InputTokens
					muCache = fmt.Sprintf(", %d%% cached", pct)
				}
				fmt.Fprintf(r.w, "  %-35s %s in / %s out  (%d %s%s)\n",
					mu.Model,
					formatTokens(mu.InputTokens), formatTokens(mu.OutputTokens),
					mu.TotalCalls, plural("call", mu.TotalCalls), muCache)
			}
		}
	}
}

func taskIcon(status string) (string, string) {
	switch status {
	case "completed":
		return "✓", Green
	case "running":
		return "⠸", Cyan
	case "failed":
		return "✗", Red
	case "cancelled":
		return "⊘", Gray
	default:
		return "·", Gray
	}
}

func statusLabel(status string) string {
	switch status {
	case "running":
		return "working..."
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "waiting"
	}
}

// animatedStatusLabel returns a witty rotating message for running tasks,
// or the standard label for other statuses.
func animatedStatusLabel(status string, frame int) string {
	if status == "running" {
		return WittyStatus(frame)
	}
	return statusLabel(status)
}

func statusPrefix(status string) string {
	switch status {
	case "completed":
		return "OK "
	case "failed":
		return "ERR"
	case "running":
		return "..."
	case "cancelled":
		return "CXL"
	default:
		return "   "
	}
}

func tokenSuffix(taskID string, tokenMap map[string]taskTokens) string {
	tok, ok := tokenMap[taskID]
	if !ok || (tok.inputTokens == 0 && tok.outputTokens == 0) {
		return ""
	}
	return fmt.Sprintf(" (%s in / %s out)", formatTokens(tok.inputTokens), formatTokens(tok.outputTokens))
}

func formatTokens(n int64) string {
	if n < 0 {
		return "-" + formatTokens(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	remainder := len(s) % 3
	if remainder > 0 {
		b.WriteString(s[:remainder])
	}
	for i := remainder; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func plural(word string, n int64) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func hasRunningTasks(tasks []event.TaskSummary) bool {
	for _, t := range tasks {
		if t.Status == "running" {
			return true
		}
	}
	return false
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func visibleLength(s string) int {
	return runewidth.StringWidth(ansiRegex.ReplaceAllString(s, ""))
}

// truncateVisual truncates a string containing ANSI escape codes to maxVisible
// visible characters, preserving escape sequences and appending Reset.
func truncateVisual(s string, maxVisible int) string {
	if maxVisible <= 0 {
		return ""
	}
	var b strings.Builder
	vis := 0
	i := 0
	for i < len(s) && vis < maxVisible {
		// Skip ANSI escape sequences (they don't consume visible width).
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++ // include 'm'
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := runewidth.RuneWidth(r)
		if vis+rw > maxVisible {
			break
		}
		b.WriteString(s[i : i+size])
		vis += rw
		i += size
	}
	b.WriteString(Reset)
	return b.String()
}

func renderedPhysicalRows(lines []string, termWidth int) int {
	if len(lines) == 0 {
		return 0
	}
	if termWidth <= 0 {
		termWidth = 80
	}
	total := 0
	for _, line := range lines {
		total += physicalLines(visibleLength(line), termWidth)
	}
	return total
}

func tailSnippet(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[len(s)-maxLen:]
	}
	return "..." + s[len(s)-(maxLen-3):]
}

type resultBlock struct {
	taskID string
	agent  string
	text   string
}

var resultBlockRegex = regexp.MustCompile(`\*\*(\w+)\*\*\s+\((\w[\w-]*)\):`)

func parseResultBlocks(s string) []resultBlock {
	locs := resultBlockRegex.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return nil
	}
	var blocks []resultBlock
	for i, loc := range locs {
		taskID := s[loc[2]:loc[3]]
		agent := s[loc[4]:loc[5]]
		textStart := loc[1]
		textEnd := len(s)
		if i+1 < len(locs) {
			textEnd = locs[i+1][0]
		}
		text := strings.TrimSpace(s[textStart:textEnd])
		text = strings.TrimRight(text, "- \n\r")
		text = strings.TrimSpace(text)
		blocks = append(blocks, resultBlock{taskID: taskID, agent: agent, text: text})
	}
	return blocks
}

// Pause signals the renderer to clear its animation and block until the erase
// is complete. The caller can safely write to the terminal after Pause returns.
func (r *Renderer) Pause() {
	r.pauseCh <- struct{}{}
	<-r.pauseAckCh
}

// Resume unblocks the renderer after a Pause.
func (r *Renderer) Resume() {
	r.resumeCh <- struct{}{}
}

// Glamour returns the glamour renderer (for external chat rendering).
func (r *Renderer) Glamour() *glamour.TermRenderer {
	return r.glamour
}

// TTY returns whether this renderer targets a TTY.
func (r *Renderer) TTY() bool {
	return r.tty
}

// Writer returns the underlying writer.
func (r *Renderer) Writer() io.Writer {
	return r.w
}

// RenderResults formats and prints the result output from collectResults.
// TTY: structured blocks with agent colors and indentation.
// Non-TTY: plain taskID (agent): text format.
func (r *Renderer) RenderResults(result string) {
	blocks := parseResultBlocks(result)
	if len(blocks) == 0 {
		fmt.Fprintln(r.w, result)
		return
	}

	if r.tty {
		r.drawRule("Results")
		fmt.Fprintln(r.w)
		for _, b := range blocks {
			color := r.assignColor(b.agent)
			fmt.Fprintf(r.w, "  %s%s%s  %s%s%s\n",
				Bold, b.taskID, Reset,
				color, b.agent, Reset)
			fmt.Fprintf(r.w, "  %s%s%s\n", Gray, strings.Repeat(BoxHorizontal, 16), Reset)
			body := collapseBlankLines(r.renderMarkdown(b.text))
			fmt.Fprintln(r.w, indentBlock(body, "  "))
			fmt.Fprintln(r.w)
		}
	} else {
		for _, b := range blocks {
			fmt.Fprintf(r.w, "%s (%s):\n%s\n\n", b.taskID, b.agent, b.text)
		}
	}
}

// RenderSummary prints the cached completion summary from the last RenderRequest.
func (r *Renderer) RenderSummary() {
	if r.lastSummary != nil {
		r.printSummary(*r.lastSummary)
		r.lastSummary = nil
	}
}

// RenderSeparator prints a blank rule as a visual divider between request cycles.
func (r *Renderer) RenderSeparator() {
	r.drawRule("")
	fmt.Fprintln(r.w)
}
