package ui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProjectInfo holds project metadata for the interactive project picker.
type ProjectInfo struct {
	Name       string
	Dir        string
	LastUsedAt time.Time
	Error      string
}

// PickProject shows a project selector.
// Returns the selected project name, or "new" if the user wants to create a
// new project. Returns "" on cancel.
func PickProject(w io.Writer, projects []ProjectInfo, readLine func() (string, bool), tty bool, ti *TermInput) string {
	if tty && ti != nil {
		if result, ok := pickProjectArrow(w, projects, ti); ok {
			return result
		}
		// Arrow mode failed, fall through to text.
	}
	return pickProjectText(w, projects, readLine, tty)
}

// pickProjectArrow renders an arrow-navigable project picker.
func pickProjectArrow(w io.Writer, projects []ProjectInfo, ti *TermInput) (string, bool) {
	// Options: each project + "Create new project" at the end.
	total := len(projects) + 1
	selected := 0

	fmt.Fprintln(w) // separator
	lines := drawProjectPicker(w, projects, selected)

	if err := ti.EnterRaw(); err != nil {
		eraseLines(w, lines+1)
		return "", false
	}
	keyCh := ti.StartKeyEvents()

	for {
		key, ok := <-keyCh
		if !ok {
			break
		}
		switch {
		case key.Key == "up":
			if selected > 0 {
				selected--
				eraseLines(w, lines)
				lines = drawProjectPicker(w, projects, selected)
			}
		case key.Key == "down":
			if selected < total-1 {
				selected++
				eraseLines(w, lines)
				lines = drawProjectPicker(w, projects, selected)
			}
		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			if selected == len(projects) {
				fmt.Fprintf(w, "  %s+ Create new project%s\n", Green, Reset)
				return "new", true
			}
			name := projects[selected].Name
			fmt.Fprintf(w, "  %s▸ %s%s\n", Cyan, name, Reset)
			return name, true
		case key.Key == "esc":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			return "", true
		case key.Key == "ctrl-c":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			if ti.ConfirmQuit(w) {
				eraseLines(w, lines)
				return "", true
			}
			// Re-enter raw mode and redraw.
			ti.EnterRaw()
			keyCh = ti.StartKeyEvents()
			eraseLines(w, lines)
			lines = drawProjectPicker(w, projects, selected)
		case key.Rune >= '1' && key.Rune <= '9':
			idx := int(key.Rune - '1')
			if idx < total {
				ti.StopKeyEvents()
				ti.Drain()
				ti.ExitRaw()
				eraseLines(w, lines)
				if idx == len(projects) {
					fmt.Fprintf(w, "  %s+ Create new project%s\n", Green, Reset)
					return "new", true
				}
				name := projects[idx].Name
				fmt.Fprintf(w, "  %s▸ %s%s\n", Cyan, name, Reset)
				return name, true
			}
		}
	}

	ti.ExitRaw()
	return "", true
}

// drawProjectPicker renders the project list with highlighted selection.
// Returns the number of lines drawn.
func drawProjectPicker(w io.Writer, projects []ProjectInfo, selected int) int {
	fmt.Fprintf(w, "  %sSelect a Project%s\n", Bold, Reset)
	lines := 1

	for i, p := range projects {
		prefix := "   "
		nameStyle := ""
		nameEnd := ""
		if i == selected {
			prefix = Cyan + " ▸ " + Reset
			nameStyle = Bold
			nameEnd = Reset
		}

		dir := abbreviateHome(p.Dir)
		ago := relativeTime(p.LastUsedAt)
		if p.Error != "" {
			fmt.Fprintf(w, " %s%s%s%s  %s%-30s%s  %s(%s · %s)%s\n",
				prefix, nameStyle, p.Name, nameEnd,
				Gray, dir, Reset,
				Dim, ago, p.Error, Reset)
		} else {
			fmt.Fprintf(w, " %s%s%s%s  %s%-30s%s  %s(%s)%s\n",
				prefix, nameStyle, p.Name, nameEnd,
				Gray, dir, Reset,
				Dim, ago, Reset)
		}
		lines++
	}

	// "Create new project" option.
	if selected == len(projects) {
		fmt.Fprintf(w, " %s ▸ %s%s+ Create new project%s\n", Cyan, Reset, Green+Bold, Reset)
	} else {
		fmt.Fprintf(w, "     %s+ Create new project%s\n", Green, Reset)
	}
	lines++

	// Hint line.
	fmt.Fprintf(w, "  %s↑↓%s navigate  %sEnter%s select  %sEsc%s cancel\n",
		Bold, Reset+Dim, Bold, Reset+Dim, Bold, Reset+Dim+Reset)
	lines++

	return lines
}

// pickProjectText is the fallback numbered-list picker.
func pickProjectText(w io.Writer, projects []ProjectInfo, readLine func() (string, bool), tty bool) string {
	if tty {
		fmt.Fprintln(w)
		for i, p := range projects {
			dir := abbreviateHome(p.Dir)
			ago := relativeTime(p.LastUsedAt)
			if p.Error != "" {
				fmt.Fprintf(w, "  %s[%d]%s %s  %s%s%s  %s(%s · %s)%s\n",
					Dim, i+1, Reset,
					p.Name,
					Gray, dir, Reset,
					Dim, ago, p.Error, Reset)
			} else {
				fmt.Fprintf(w, "  %s[%d]%s %s  %s%s%s  %s(%s)%s\n",
					Dim, i+1, Reset,
					p.Name,
					Gray, dir, Reset,
					Dim, ago, Reset)
			}
		}
		fmt.Fprintf(w, "  %s[n]%s + Create new project\n", Dim, Reset)
		fmt.Fprintf(w, "\n  %sSelect project [1-%d, n]:%s ", Bold, len(projects), Reset)
	} else {
		fmt.Fprintln(w, "Select a project:")
		for i, p := range projects {
			dir := abbreviateHome(p.Dir)
			fmt.Fprintf(w, "[%d] %s (%s)\n", i+1, p.Name, dir)
		}
		fmt.Fprintln(w, "[n] Create new project")
		fmt.Fprint(w, "Selection: ")
	}

	input, ok := readLine()
	if !ok {
		return ""
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	if input == "n" || input == "N" || input == "new" {
		return "new"
	}

	idx, err := strconv.Atoi(input)
	if err != nil || idx < 1 || idx > len(projects) {
		return ""
	}
	return projects[idx-1].Name
}

// abbreviateHome replaces the home directory prefix with ~.
func abbreviateHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// relativeTime returns a human-readable relative time string.
func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	}
}
