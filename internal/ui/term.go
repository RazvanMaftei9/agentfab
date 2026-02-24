package ui

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"
)

const (
	Reset     = "\033[0m"
	Bold      = "\033[1m"
	Dim       = "\033[2m"
	Green     = "\033[38;2;0;186;93m"   // #00ba5d
	Yellow    = "\033[38;2;252;208;8m"  // #fcd008
	Red       = "\033[38;2;255;61;94m"  // #ff3d5e
	Blue      = "\033[38;2;31;103;237m" // #1f67ed
	Teal      = "\033[38;2;4;209;172m"  // #04d1ac
	Magenta   = "\033[35m"
	Cyan      = "\033[36m"
	White     = "\033[97m"
	LogoGreen = "\033[38;2;17;186;62m" // #11ba3e
	Gray      = "\033[90m"

	BrightRed     = "\033[91m"
	BrightGreen   = "\033[92m"
	BrightYellow  = "\033[93m"
	BrightBlue    = "\033[94m"
	BrightMagenta = "\033[95m"
	BrightCyan    = "\033[96m"

	ClearScreen = "\033[2J\033[H"
	ClearLn     = "\033[2K"
	ClearDown   = "\033[J" // erase from cursor to end of display
	MoveUp      = "\033[1A"
)

const (
	BoxTopLeft     = "╭"
	BoxTopRight    = "╮"
	BoxBottomLeft  = "╰"
	BoxBottomRight = "╯"
	BoxHorizontal  = "─"
	BoxVertical    = "│"
)

const (
	ConnArrow    = "▸"
	ConnTeeDown  = "┬"
	ConnTeeUp    = "┴"
	ConnTeeRight = "├"
	ConnTeeLeft  = "┤"
	ConnCross    = "┼"
	ConnTopLeft  = "┌"
	ConnTopRight = "┐"
	ConnBotLeft  = "└"
	ConnBotRight = "┘"
)

const (
	DirUp    uint8 = 1 << iota // 1
	DirDown                    // 2
	DirLeft                    // 4
	DirRight                   // 8
)

// IsTTY returns true if f is a terminal.
func IsTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// TermWidth returns the terminal width, defaulting to 80 if detection fails.
func TermWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// TermHeight returns the terminal height, defaulting to 24 if detection fails.
func TermHeight() int {
	_, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || h <= 0 {
		return 24
	}
	return h
}

// EraseBlock moves the cursor up n lines and clears everything from the cursor
// to the end of the display. This is more robust than clearing individual lines
// because it handles line-count drift (e.g., snippet bar changes) and terminal
// scrolling edge cases that can leave ghost lines.
func EraseBlock(w io.Writer, n int) {
	if n <= 0 {
		return
	}
	fmt.Fprintf(w, "\r\033[%dA", n)
	fmt.Fprint(w, ClearDown)
}

// Spinner shows a braille-dot animation with a message.
type Spinner struct {
	w      io.Writer
	tty    bool
	mu     sync.Mutex
	msg    string
	stopCh chan struct{}
	done   chan struct{}
}

var frames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

// NewSpinner creates a spinner that writes to w.
func NewSpinner(w io.Writer, tty bool) *Spinner {
	return &Spinner{
		w:      w,
		tty:    tty,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Start begins the spinner animation with the given message.
func (s *Spinner) Start(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()

	if !s.tty {
		fmt.Fprintf(s.w, "... %s\n", msg)
		close(s.done)
		return
	}

	go func() {
		defer close(s.done)
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			s.mu.Lock()
			m := s.msg
			s.mu.Unlock()
			fmt.Fprintf(s.w, "%s\r%s%c%s %s", ClearLn, Cyan, frames[i%len(frames)], Reset, m)
			i++
			select {
			case <-s.stopCh:
				fmt.Fprintf(s.w, "%s\r", ClearLn)
				return
			case <-tick.C:
			}
		}
	}()
}

// Update changes the spinner message.
func (s *Spinner) Update(msg string) {
	s.mu.Lock()
	s.msg = msg
	s.mu.Unlock()
}

// Msg returns the current spinner message.
func (s *Spinner) Msg() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msg
}

// Stop halts the spinner and clears the line.
func (s *Spinner) Stop() {
	select {
	case <-s.stopCh:
		// Already stopped.
	default:
		close(s.stopCh)
	}
	<-s.done
}

var wittyMessages = []string{
	"analyzing task graph...",
	"synchronizing agent states...",
	"routing messages through Conductor...",
	"evaluating constraints...",
	"synthesizing logic...",
	"consulting the knowledge graph...",
	"resolving dependencies...",
	"compiling context windows...",
	"simulating agent consensus...",
	"inspecting artifact boundaries...",
	"negotiating with LLM...",
	"rehydrating knowledge vectors...",
}

// WittyStatus returns a rotating witty status message based on the frame counter.
// The message changes every ~3s at 150ms tick (every 20 frames).
func WittyStatus(frame int) string {
	idx := (frame / 20) % len(wittyMessages)
	return wittyMessages[idx]
}
