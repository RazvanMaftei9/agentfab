package ui

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/charmbracelet/glamour"
)

// AgentInfo holds agent status for the interactive chat picker.
type AgentInfo struct {
	Name     string
	Model    string
	Status   string // "conductor", "running", "idle"
	TaskID   string
	TaskDesc string
}

// PickAgent displays an interactive agent picker.
// With ti != nil and tty=true, uses arrow-key navigation.
// Falls back to numbered list if cbreak mode is unavailable.
func PickAgent(w io.Writer, agents []AgentInfo, readLine func() (string, bool), tty bool, ti *TermInput) string {
	if len(agents) == 0 {
		return ""
	}
	if tty && ti != nil {
		if result, ok := pickAgentArrow(w, agents, ti); ok {
			return result
		}
	}
	return pickAgentText(w, agents, readLine, tty)
}

func pickAgentArrow(w io.Writer, agents []AgentInfo, ti *TermInput) (string, bool) {
	selected := 0

	fmt.Fprintln(w)
	lines := drawAgentPicker(w, agents, selected)

	if err := ti.EnterRaw(); err != nil {
		eraseLines(w, lines+1)
		return "", false
	}
	keyCh := ti.StartKeyEvents()

	for key := range keyCh {
		switch {
		case key.Key == "up":
			if selected > 0 {
				selected--
				eraseLines(w, lines)
				lines = drawAgentPicker(w, agents, selected)
			}
		case key.Key == "down":
			if selected < len(agents)-1 {
				selected++
				eraseLines(w, lines)
				lines = drawAgentPicker(w, agents, selected)
			}
		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			name := agents[selected].Name
			fmt.Fprintf(w, "  %s▸ %s%s\n", Cyan, name, Reset)
			return name, true
		case key.Key == "esc" || key.Key == "ctrl-c":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			return "", true
		case key.Rune >= '1' && key.Rune <= '9':
			idx := int(key.Rune - '1')
			if idx < len(agents) {
				ti.StopKeyEvents()
				ti.Drain()
				ti.ExitRaw()
				eraseLines(w, lines)
				name := agents[idx].Name
				fmt.Fprintf(w, "  %s▸ %s%s\n", Cyan, name, Reset)
				return name, true
			}
		}
	}

	ti.ExitRaw()
	return "", true
}

func drawAgentPicker(w io.Writer, agents []AgentInfo, selected int) int {
	for i, a := range agents {
		prefix := "   "
		nameStyle := ""
		nameEnd := ""
		if i == selected {
			prefix = Cyan + " ▸ " + Reset
			nameStyle = Bold
			nameEnd = Reset
		}

		statusIcon := "·"
		statusColor := Gray
		suffix := Dim + "idle" + Reset
		switch a.Status {
		case "conductor":
			statusIcon = "●"
			statusColor = Cyan
			suffix = Gray + "(conductor)" + Reset
		case "running":
			statusIcon = "●"
			statusColor = Cyan
			suffix = Gray + "running: " + a.TaskID + Reset
		}

		fmt.Fprintf(w, " %s%s%s%s %s%s%s  %s\n",
			prefix, nameStyle, a.Name, nameEnd,
			statusColor, statusIcon, Reset,
			suffix)
	}

	fmt.Fprintf(w, "  %s↑↓%s navigate  %sEnter%s select  %sEsc%s cancel\n",
		Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim+Reset)

	return len(agents) + 1
}

func pickAgentText(w io.Writer, agents []AgentInfo, readLine func() (string, bool), tty bool) string {
	if tty {
		fmt.Fprintln(w)
		for i, a := range agents {
			color := ""
			icon := "·"
			suffix := ""
			switch a.Status {
			case "conductor":
				color = Cyan
				icon = "●"
				suffix = "  " + Gray + "(conductor)" + Reset
			case "running":
				color = Blue
				icon = "●"
				suffix = "  " + Gray + "running: " + a.TaskID + Reset
			default:
				color = Gray
				icon = "·"
				suffix = "  " + Dim + "idle" + Reset
			}
			fmt.Fprintf(w, "  %s[%d]%s %s%s%s %s%s\n",
				Dim, i+1, Reset,
				color, icon, Reset,
				a.Name, suffix)
		}
		fmt.Fprintf(w, "\n  %sSelect agent [1-%d]:%s ", Bold, len(agents), Reset)
	} else {
		for i, a := range agents {
			suffix := ""
			switch a.Status {
			case "conductor":
				suffix = " (conductor)"
			case "running":
				suffix = " (running " + a.TaskID + ")"
			default:
				suffix = " (idle)"
			}
			fmt.Fprintf(w, "[%d] %s%s\n", i+1, a.Name, suffix)
		}
		fmt.Fprint(w, "Select agent: ")
	}

	input, ok := readLine()
	if !ok {
		return ""
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}

	n, err := strconv.Atoi(input)
	if err != nil || n < 1 || n > len(agents) {
		return ""
	}
	return agents[n-1].Name
}

// PromptReply shows y/n quick-reply options. Other keys start free-text input.
func PromptReply(w io.Writer, readLine func() (string, bool), tty bool, ti *TermInput) string {
	if !tty || ti == nil {
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		return strings.TrimSpace(line)
	}

	fmt.Fprintf(w, "  %sy%s Yes  %sn%s No  %sEnter%s type  %sEsc%s return\n",
		Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim+Reset)

	if err := ti.EnterRaw(); err != nil {
		eraseLines(w, 1)
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		return strings.TrimSpace(line)
	}
	keyCh := ti.StartKeyEvents()

	for key := range keyCh {
		switch {
		case key.Rune == 'y' || key.Rune == 'Y':
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, 1)
			fmt.Fprintf(w, "  %s>%s Yes\n", Dim, Reset)
			return "Yes"

		case key.Rune == 'n' || key.Rune == 'N':
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, 1)
			fmt.Fprintf(w, "  %s>%s No\n", Dim, Reset)
			return "No"

		case key.Key == "esc" || key.Key == "ctrl-c":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, 1)
			return ""

		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			eraseLines(w, 1)
			line, ok := ti.ReadLineInit(w, "  "+Bold+">"+Reset+" ", nil)
			ti.ExitRaw()
			if !ok {
				return ""
			}
			return strings.TrimSpace(line)

		default:
			if key.Rune > 0 {
				ti.StopKeyEvents()
				ti.Drain()
				eraseLines(w, 1)
				initial := []byte{byte(key.Rune)}
				line, ok := ti.ReadLineInit(w, "  "+Bold+">"+Reset+" ", initial)
				ti.ExitRaw()
				if !ok {
					return ""
				}
				return strings.TrimSpace(line)
			}
		}
	}

	ti.ExitRaw()
	return ""
}

// PromptReplyWithOptions shows numbered suggestion options with free-text fallback.
func PromptReplyWithOptions(w io.Writer, options []string, readLine func() (string, bool), tty bool, ti *TermInput) string {
	if len(options) == 0 {
		return PromptReply(w, readLine, tty, ti)
	}

	if !tty || ti == nil {
		for i, opt := range options {
			fmt.Fprintf(w, "  [%d] %s\n", i+1, opt)
		}
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		line = strings.TrimSpace(line)
		n, err := strconv.Atoi(line)
		if err == nil && n >= 1 && n <= len(options) {
			return options[n-1]
		}
		return line
	}

	lines := 0
	for i, opt := range options {
		fmt.Fprintf(w, "  %s[%d]%s %s\n", Bold, i+1, Reset, opt)
		lines++
	}
	fmt.Fprintf(w, "  %s1-%d%s select  %sEnter%s type  %sEsc%s return\n",
		Bold, len(options), Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim+Reset)
	lines++

	if err := ti.EnterRaw(); err != nil {
		eraseLines(w, lines)
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		return strings.TrimSpace(line)
	}
	keyCh := ti.StartKeyEvents()

	for key := range keyCh {
		switch {
		case key.Rune >= '1' && key.Rune <= '9':
			idx := int(key.Rune - '1')
			if idx < len(options) {
				ti.StopKeyEvents()
				ti.Drain()
				ti.ExitRaw()
				eraseLines(w, lines)
				fmt.Fprintf(w, "  %s>%s %s\n", Dim, Reset, options[idx])
				return options[idx]
			}

		case key.Rune == 'y' || key.Rune == 'Y':
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			fmt.Fprintf(w, "  %s>%s Yes\n", Dim, Reset)
			return "Yes"

		case key.Rune == 'n' || key.Rune == 'N':
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			fmt.Fprintf(w, "  %s>%s No\n", Dim, Reset)
			return "No"

		case key.Key == "esc" || key.Key == "ctrl-c":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			return ""

		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			eraseLines(w, lines)
			line, ok := ti.ReadLineInit(w, "  "+Bold+">"+Reset+" ", nil)
			ti.ExitRaw()
			if !ok {
				return ""
			}
			return strings.TrimSpace(line)

		default:
			if key.Rune > 0 {
				ti.StopKeyEvents()
				ti.Drain()
				eraseLines(w, lines)
				initial := []byte{byte(key.Rune)}
				line, ok := ti.ReadLineInit(w, "  "+Bold+">"+Reset+" ", initial)
				ti.ExitRaw()
				if !ok {
					return ""
				}
				return strings.TrimSpace(line)
			}
		}
	}

	ti.ExitRaw()
	return ""
}

func eraseLines(w io.Writer, n int) {
	if n > 0 {
		fmt.Fprint(w, strings.Repeat(MoveUp+ClearLn, n))
	}
}

// RenderChatResponse displays an agent's chat response.
func RenderChatResponse(w io.Writer, agentName, response string, tty bool, glamourRenderer *glamour.TermRenderer) {
	if tty {
		fmt.Fprintf(w, "\n  %s%s%s%s\n", Bold, Cyan, agentName, Reset)
		if glamourRenderer != nil {
			rendered, err := glamourRenderer.Render(response)
			if err == nil {
				fmt.Fprintln(w, indentBlock(strings.TrimRight(rendered, "\n"), "  "))
			} else {
				fmt.Fprintln(w, indentBlock(response, "  "))
			}
		} else {
			fmt.Fprintln(w, indentBlock(response, "  "))
		}
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "%s: %s\n", agentName, response)
	}
}

// RenderAgentQuery displays an agent's question to the user.
func RenderAgentQuery(w io.Writer, agentName, question string, tty bool) {
	if tty {
		fmt.Fprintf(w, "\n  %s?%s %s%s%s asks:\n", Yellow, Reset, Bold, agentName, Reset)
		fmt.Fprintf(w, "  %s\n\n", question)
	} else {
		fmt.Fprintf(w, "%s asks: %s\n", agentName, question)
	}
}

// PromptFreeText shows a "> " text input prompt without y/n quick-reply shortcuts.
func PromptFreeText(w io.Writer, readLine func() (string, bool), tty bool, ti *TermInput) string {
	if !tty || ti == nil {
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		return strings.TrimSpace(line)
	}

	if err := ti.EnterRaw(); err != nil {
		fmt.Fprint(w, "> ")
		line, ok := readLine()
		if !ok {
			return ""
		}
		return strings.TrimSpace(line)
	}
	line, ok := ti.ReadMultiLine(w, "  "+Bold+">"+Reset+" ", "")
	ti.ExitRaw()
	if !ok {
		return ""
	}
	return strings.TrimSpace(line)
}
