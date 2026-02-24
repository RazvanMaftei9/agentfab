package ui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const escapeTimeout = 150 * time.Millisecond

// KeyEvent represents a single keypress in cbreak terminal mode.
type KeyEvent struct {
	Rune rune   // printable character, 0 if special key
	Key  string // "enter", "tab", "esc", "backspace", "ctrl-c", "ctrl-d", "up", "down", "left", "right"
}

// TermInput reads from the terminal in cbreak mode via /dev/tty with blocking
// unix.Read pinned to an OS thread, bypassing Go's poller which stalls after
// terminal mode transitions.
type TermInput struct {
	fd        int
	ttyFd     int // /dev/tty fd for blocking reads; -1 if unavailable
	origState *term.State
	byteCh    chan byte
	stopCh chan struct{}
	doneCh chan struct{} // closed when key event goroutine exits
	raw       bool          // true = key event mode (no echo); false = line input mode (manual echo)

	// OnQuit is called when the user confirms quit via Ctrl+C → y.
	// If nil, Ctrl+C behaves as before (cancel current input).
	OnQuit func()
}

// NewTermInput creates a terminal input handler. Enters cbreak mode
// immediately and opens /dev/tty for blocking reads if stdin is a TTY.
func NewTermInput() *TermInput {
	fd := int(os.Stdin.Fd())
	origState, _ := term.GetState(fd)

	ttyFd := -1
	if IsTTY(os.Stdin) {
		// Enter cbreak mode BEFORE opening /dev/tty or starting the read
		// loop. This ensures the first unix.Read sees cbreak mode, so every
		// keystroke is delivered immediately without waiting for Enter.
		enterCbreak(fd)

		// Open /dev/tty in non-blocking mode first so we can drain any
		// stale bytes from the terminal's input buffer. TCSAFLUSH on
		// stdin (fd 0) flushes the queue at that instant, but bytes can
		// arrive between the flush and this Open (e.g. from the shell's
		// Enter that launched the process). A non-blocking drain here
		// guarantees the fd is clean before readLoopTTY starts.
		if f, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); err == nil {
			// Drain any pending input.
			var discard [256]byte
			for {
				if _, err := unix.Read(f, discard[:]); err != nil {
					break
				}
			}
			// Switch to blocking mode for readLoopTTY.
			unix.SetNonblock(f, false)
			ttyFd = f
		}
	}

	ti := &TermInput{
		fd:        fd,
		ttyFd:     ttyFd,
		origState: origState,
		byteCh:    make(chan byte, 256),
		raw:       ttyFd >= 0, // start in raw mode if we have a tty
	}

	go ti.readLoop()
	return ti
}

func (ti *TermInput) readLoop() {
	if ti.ttyFd >= 0 {
		ti.readLoopTTY()
	} else {
		ti.readLoopStdio()
	}
}

func (ti *TermInput) readLoopTTY() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	buf := make([]byte, 256)
	for {
		n, err := unix.Read(ti.ttyFd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			close(ti.byteCh)
			return
		}
		if n == 0 {
			close(ti.byteCh)
			return
		}
		for i := 0; i < n; i++ {
			ti.byteCh <- buf[i]
		}
	}
}

func (ti *TermInput) readLoopStdio() {
	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			close(ti.byteCh)
			return
		}
		for i := 0; i < n; i++ {
			ti.byteCh <- buf[i]
		}
	}
}

// enterCbreak puts the terminal into cbreak mode (no echo, char-at-a-time,
// flow control disabled, TCSAFLUSH to discard stale cooked-mode input).
func enterCbreak(fd int) error {
	termios, err := unix.IoctlGetTermios(fd, termIoctlGet)
	if err != nil {
		return err
	}
	termios.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	termios.Iflag &^= unix.IXON | unix.ICRNL // Disable flow control and CR→NL translation.
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0
	// TCSAFLUSH: apply settings after flushing the kernel input queue,
	// preventing stale cooked-mode bytes from being misinterpreted.
	return unix.IoctlSetTermios(fd, termIoctlSetFlush, termios)
}

// EnterRaw signals that callers want key-event-style input (no echo).
// The terminal is already in cbreak mode from construction; this just
// sets the flag. On a mode transition (raw was false), it drains stale
// bytes that accumulated while the flag was off. If raw is already true,
// no drain is needed — callers that want an explicit drain (e.g.
// pickProjectArrow) call Drain() themselves.
func (ti *TermInput) EnterRaw() error {
	if ti.raw {
		return nil
	}
	ti.raw = true
	ti.Drain()
	return nil
}

// ExitRaw signals that callers are done with key-event input.
// The terminal stays in cbreak mode (we handle echo in readLineRaw).
func (ti *TermInput) ExitRaw() {
	ti.raw = false
}

// IsRaw returns whether key-event mode is active.
func (ti *TermInput) IsRaw() bool {
	return ti.raw
}

// Drain discards any pending bytes in the channel buffer.
func (ti *TermInput) Drain() {
	for {
		select {
		case <-ti.byteCh:
		default:
			return
		}
	}
}

// Close restores the terminal to its original state and stops the read loop.
// Closing the /dev/tty fd interrupts the blocking unix.Read in readLoopTTY.
func (ti *TermInput) Close() {
	ti.StopKeyEvents()
	// Restore original terminal state (cbreak → cooked).
	if ti.origState != nil {
		term.Restore(ti.fd, ti.origState)
	}
	ti.raw = false
	if ti.ttyFd >= 0 {
		unix.Close(ti.ttyFd)
		ti.ttyFd = -1
	}
}

// ConfirmQuit shows "Quit? (y/n)" and reads a single byte response.
// Returns true if the user confirms with 'y' or 'Y'.
// If OnQuit is set and the user confirms, calls OnQuit and returns true.
// If OnQuit is nil, always returns false (legacy cancel behavior).
func (ti *TermInput) ConfirmQuit(w io.Writer) bool {
	if ti.OnQuit == nil {
		return false
	}
	fmt.Fprintf(w, "\n  %sQuit? (y/n)%s ", Yellow, Reset)
	for {
		select {
		case b, ok := <-ti.byteCh:
			if !ok {
				return false
			}
			switch {
			case b == 'y' || b == 'Y':
				fmt.Fprintf(w, "y\n")
				ti.OnQuit()
				return true
			case b == 'n' || b == 'N', b == 0x1b, b == '\r', b == '\n':
				fmt.Fprintf(w, "n\n")
				return false
			case b == 0x03: // Another Ctrl+C while prompting — treat as force quit
				fmt.Fprintf(w, "\n")
				ti.OnQuit()
				return true
			}
		case <-time.After(10 * time.Second):
			fmt.Fprintf(w, "n\n")
			return false
		}
	}
}

// ReadLine reads a complete line from the terminal.
// Since the terminal is permanently in cbreak mode, this always uses
// manual echo and line editing (backspace, Ctrl+C, Ctrl+U, etc.).
func (ti *TermInput) ReadLine(w io.Writer, prompt string) (string, bool) {
	return ti.readLineRaw(w, prompt)
}

func (ti *TermInput) readLineRaw(w io.Writer, prompt string) (string, bool) {
	if prompt != "" {
		fmt.Fprint(w, prompt)
	}
	var buf []byte
	for {
		b, ok := <-ti.byteCh
		if !ok {
			return "", false
		}
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(w, "\n")
			return string(buf), true
		case b == 0x7f || b == 0x08: // backspace / delete
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Fprint(w, "\b \b")
			}
		case b == 0x03: // Ctrl+C
			if ti.ConfirmQuit(w) {
				return "", false
			}
			// Not quitting — redraw prompt and buffer.
			if prompt != "" {
				fmt.Fprint(w, prompt)
			}
			fmt.Fprint(w, string(buf))
			continue
		case b == 0x04: // Ctrl+D (EOF)
			return "", false
		case b == 0x15: // Ctrl+U (clear line)
			for range buf {
				fmt.Fprint(w, "\b \b")
			}
			buf = buf[:0]
		case b == 0x1b: // ESC — consume escape sequence silently
			ti.consumeEscape()
		case b >= 0x20 && b < 0x7f: // printable ASCII
			buf = append(buf, b)
			fmt.Fprint(w, string(rune(b)))
		}
	}
}

// ReadLineInit reads a line in cbreak mode with a prompt and optional initial content.
// The initial bytes are echoed and pre-populate the buffer. Must be in raw mode.
func (ti *TermInput) ReadLineInit(w io.Writer, prompt string, initial []byte) (string, bool) {
	if prompt != "" {
		fmt.Fprint(w, prompt)
	}
	buf := make([]byte, 0, 128)
	if len(initial) > 0 {
		buf = append(buf, initial...)
		fmt.Fprint(w, string(initial))
	}
	for {
		b, ok := <-ti.byteCh
		if !ok {
			return "", false
		}
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(w, "\n")
			return string(buf), true
		case b == 0x7f || b == 0x08: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Fprint(w, "\b \b")
			}
		case b == 0x03: // Ctrl+C
			if ti.ConfirmQuit(w) {
				return "", false
			}
			if prompt != "" {
				fmt.Fprint(w, prompt)
			}
			fmt.Fprint(w, string(buf))
			continue
		case b == 0x04: // Ctrl+D
			return "", false
		case b == 0x15: // Ctrl+U — clear line
			for range buf {
				fmt.Fprint(w, "\b \b")
			}
			buf = buf[:0]
		case b == 0x1b: // ESC — consume escape sequence silently
			ti.consumeEscape()
		case b >= 0x20 && b < 0x7f: // printable ASCII
			buf = append(buf, b)
			fmt.Fprint(w, string(rune(b)))
		}
	}
}

// ReadLinePlaceholder reads a line with a dimmed placeholder that vanishes on first keystroke.
func (ti *TermInput) ReadLinePlaceholder(w io.Writer, prompt string, placeholder string) (string, bool) {
	if prompt != "" {
		fmt.Fprint(w, prompt)
	}

	buf := make([]byte, 0, 128)

	fmt.Fprintf(w, "%s%s%s", Gray, placeholder, Reset)

	placeholderActive := true

	for {
		b, ok := <-ti.byteCh
		if !ok {
			return "", false
		}

		if placeholderActive {
			switch {
			case b == '\r' || b == '\n':
				fmt.Fprintf(w, "\r\033[2K")
				if prompt != "" {
					fmt.Fprint(w, prompt)
				}
				fmt.Fprint(w, "\n")
				return "", true
			case b == 0x1b:
				placeholderActive = false
				fmt.Fprintf(w, "\r\033[2K")
				if prompt != "" {
					fmt.Fprint(w, prompt)
				}
				ti.consumeEscape()
				continue
			case b == 0x03: // Ctrl+C
				if ti.ConfirmQuit(w) {
					return "", false
				}
				fmt.Fprintf(w, "\r\033[2K")
				if prompt != "" {
					fmt.Fprint(w, prompt)
				}
				fmt.Fprintf(w, "%s%s%s", Gray, placeholder, Reset)
				continue
			case b == 0x04: // Ctrl+D
				fmt.Fprint(w, "\n")
				return "", false
			case b == 0x7f || b == 0x08:
				placeholderActive = false
				fmt.Fprintf(w, "\r\033[2K")
				if prompt != "" {
					fmt.Fprint(w, prompt)
				}
				continue
			default:
				// Normal character. Erase the placeholder completely.
				placeholderActive = false
				fmt.Fprintf(w, "\r\033[2K")
				if prompt != "" {
					fmt.Fprint(w, prompt)
				}
			}
		}

		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(w, "\n")
			return string(buf), true
		case b == 0x7f || b == 0x08: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				fmt.Fprint(w, "\b \b")
			}
		case b == 0x03: // Ctrl+C
			if ti.ConfirmQuit(w) {
				return "", false
			}
			if prompt != "" {
				fmt.Fprint(w, prompt)
			}
			fmt.Fprint(w, string(buf))
			continue
		case b == 0x04: // Ctrl+D
			return "", false
		case b == 0x15: // Ctrl+U — clear line
			for range buf {
				fmt.Fprint(w, "\b \b")
			}
			buf = buf[:0]
		case b == 0x1b: // ESC — consume escape sequence silently
			ti.consumeEscape()
		case b >= 0x20 && b < 0x7f: // printable ASCII
			buf = append(buf, b)
			fmt.Fprint(w, string(rune(b)))
		}
	}
}

// ReadMultiLine reads multi-line input. Enter submits, Alt+Enter inserts a newline.
func (ti *TermInput) ReadMultiLine(w io.Writer, prompt string, placeholder string) (string, bool) {

	lines := [][]byte{{}}
	placeholderActive := placeholder != ""
	hintVisible := false

	fmt.Fprint(w, "\033[?2004h")
	defer fmt.Fprint(w, "\033[?2004l")

	if placeholderActive {
		fmt.Fprintf(w, "%s%s%s%s", prompt, Gray, placeholder, Reset)
	} else {
		fmt.Fprint(w, prompt)
	}

	for {
		b, ok := <-ti.byteCh
		if !ok {
			return "", false
		}

		if placeholderActive {
			switch {
			case b == '\r' || b == '\n':
				fmt.Fprintf(w, "\r%s\n", ClearLn)
				return "", true
			case b == 0x03: // Ctrl+C
				if ti.ConfirmQuit(w) {
					return "", false
				}
				eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
				fmt.Fprintf(w, "%s%s%s%s", prompt, Gray, placeholder, Reset)
				continue
			case b == 0x04: // Ctrl+D
				fmt.Fprintf(w, "\r%s\n", ClearLn)
				return "", false
			case b == 0x7f || b == 0x08: // Backspace
				placeholderActive = false
				eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
				redrawMultiLine(w, prompt, lines, hintVisible)
				continue
			case b == 0x15: // Ctrl+U
				placeholderActive = false
				eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
				redrawMultiLine(w, prompt, lines, hintVisible)
				continue
			case b == 0x1b:
				placeholderActive = false
				eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
				redrawMultiLine(w, prompt, lines, hintVisible)
				ti.handleMultiLineEscape(w, prompt, &lines, &hintVisible)
				continue
			default:
				if b >= 0x20 && b < 0x7f {
					placeholderActive = false
					lines[0] = append(lines[0], b)
					eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
					redrawMultiLine(w, prompt, lines, hintVisible)
					continue
				}
				continue
			}
		}

		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(w, "\n")
			return joinLines(lines), true

		case b == 0x7f || b == 0x08: // Backspace
			cur := len(lines) - 1
			prevRows := countPhysicalRows(prompt, lines, hintVisible, TermWidth())
			if len(lines[cur]) > 0 {
				lines[cur] = lines[cur][:len(lines[cur])-1]
			} else if cur > 0 {
				lines = lines[:cur]
				if len(lines) == 1 {
					hintVisible = false
				}
			}
			eraseMultiLineRegion(w, prevRows)
			redrawMultiLine(w, prompt, lines, hintVisible)

		case b == 0x03: // Ctrl+C
			if ti.ConfirmQuit(w) {
				return "", false
			}
			eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
			redrawMultiLine(w, prompt, lines, hintVisible)
			continue

		case b == 0x04: // Ctrl+D
			return "", false

		case b == 0x15: // Ctrl+U — clear current line
			cur := len(lines) - 1
			prevRows := countPhysicalRows(prompt, lines, hintVisible, TermWidth())
			lines[cur] = lines[cur][:0]
			eraseMultiLineRegion(w, prevRows)
			redrawMultiLine(w, prompt, lines, hintVisible)

		case b == 0x1b: // ESC
			ti.handleMultiLineEscape(w, prompt, &lines, &hintVisible)

		case b >= 0x20 && b < 0x7f: // Printable ASCII
			cur := len(lines) - 1
			lines[cur] = append(lines[cur], b)
			eraseMultiLineRegion(w, countPhysicalRows(prompt, lines, hintVisible, TermWidth()))
			redrawMultiLine(w, prompt, lines, hintVisible)
		}
	}
}

func (ti *TermInput) handleMultiLineEscape(w io.Writer, prompt string, lines *[][]byte, hintVisible *bool) {
	select {
	case b2 := <-ti.byteCh:
		switch {
		case b2 == '\r' || b2 == '\n':
			prevRows := countPhysicalRows(prompt, *lines, *hintVisible, TermWidth())
			*lines = append(*lines, []byte{})
			*hintVisible = true
			eraseMultiLineRegion(w, prevRows)
			redrawMultiLine(w, prompt, *lines, *hintVisible)

		case b2 == '[':
			ti.handleCSIInMultiLine(w, prompt, lines, hintVisible)

		default:
		}
	case <-time.After(escapeTimeout):
	}
}

func (ti *TermInput) handleCSIInMultiLine(w io.Writer, prompt string, lines *[][]byte, hintVisible *bool) {
	var paramBuf []byte
	for {
		select {
		case b := <-ti.byteCh:
			if b >= 0x40 && b <= 0x7e {
				if b == '~' && string(paramBuf) == "200" {
					ti.readBracketedPaste(w, prompt, lines, hintVisible)
				}
				return
			}
			paramBuf = append(paramBuf, b)
			if len(paramBuf) > 16 {
				return
			}
		case <-time.After(escapeTimeout):
			return
		}
	}
}

func (ti *TermInput) readBracketedPaste(w io.Writer, prompt string, lines *[][]byte, hintVisible *bool) {
	endMarker := []byte{0x1b, '[', '2', '0', '1', '~'}
	var recentBytes []byte

	for {
		select {
		case b, ok := <-ti.byteCh:
			if !ok {
				return
			}
			recentBytes = append(recentBytes, b)

			if len(recentBytes) >= len(endMarker) {
				tail := recentBytes[len(recentBytes)-len(endMarker):]
				if bytes.Equal(tail, endMarker) {
					pasteData := recentBytes[:len(recentBytes)-len(endMarker)]
					ti.insertPasteData(w, prompt, lines, hintVisible, pasteData)
					return
				}
			}

			if len(recentBytes) > 65536 {
				keep := len(endMarker) - 1
				pasteData := recentBytes[:len(recentBytes)-keep]
				ti.insertPasteData(w, prompt, lines, hintVisible, pasteData)
				recentBytes = recentBytes[len(recentBytes)-keep:]
			}

		case <-time.After(2 * time.Second):
			ti.insertPasteData(w, prompt, lines, hintVisible, recentBytes)
			return
		}
	}
}

func (ti *TermInput) insertPasteData(w io.Writer, prompt string, lines *[][]byte, hintVisible *bool, data []byte) {
	prevRows := countPhysicalRows(prompt, *lines, *hintVisible, TermWidth())
	for _, b := range data {
		switch {
		case b == '\r' || b == '\n':
			*lines = append(*lines, []byte{})
		case b >= 0x20 && b < 0x7f:
			cur := len(*lines) - 1
			(*lines)[cur] = append((*lines)[cur], b)
		}
	}
	if len(*lines) > 1 {
		*hintVisible = true
	}
	eraseMultiLineRegion(w, prevRows)
	redrawMultiLine(w, prompt, *lines, *hintVisible)
}

func redrawMultiLine(w io.Writer, prompt string, lines [][]byte, hintVisible bool) {
	for i, line := range lines {
		if i == 0 {
			fmt.Fprintf(w, "%s%s", prompt, string(line))
		} else {
			fmt.Fprintf(w, "\n  %s·%s %s", Dim, Reset, string(line))
		}
	}
	if hintVisible {
		fmt.Fprintf(w, "\n  %sAlt+Enter%s%s newline · %sEnter%s%s submit%s",
			Bold, Reset, Dim, Bold, Reset, Dim, Reset)
	}
}

func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEsc = false
			}
			continue
		}
		if r == '\033' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

func physicalLines(visualLen, termWidth int) int {
	if termWidth <= 0 || visualLen <= 0 {
		return 1
	}
	return (visualLen-1)/termWidth + 1
}

func countPhysicalRows(prompt string, lines [][]byte, hintVisible bool, termWidth int) int {
	total := 0
	for i, line := range lines {
		var visualLen int
		if i == 0 {
			visualLen = visibleLen(prompt) + len(line)
		} else {
			visualLen = 4 + len(line) // "  · " = 4 visible chars
		}
		total += physicalLines(visualLen, termWidth)
	}
	if hintVisible {
		total += physicalLines(37, termWidth) // hint line ~37 visible chars
	}
	return total
}

func eraseMultiLineRegion(w io.Writer, totalRows int) {
	if totalRows <= 1 {
		fmt.Fprintf(w, "\r%s", ClearLn)
		return
	}
	for i := 0; i < totalRows-1; i++ {
		fmt.Fprintf(w, "\r%s%s", ClearLn, MoveUp)
	}
	fmt.Fprintf(w, "\r%s", ClearLn)
}

func joinLines(lines [][]byte) string {
	strs := make([]string, len(lines))
	for i, l := range lines {
		strs[i] = string(l)
	}
	return strings.Join(strs, "\n")
}

func (ti *TermInput) consumeEscape() {
	select {
	case b := <-ti.byteCh:
		if b == '[' {
			for {
				select {
				case b2 := <-ti.byteCh:
					if b2 >= 0x40 && b2 <= 0x7e {
						return
					}
				case <-time.After(escapeTimeout):
					return
				}
			}
		}
	case <-time.After(escapeTimeout):
	}
}

// StartKeyEvents returns a channel of decoded key events.
// Must be in cbreak mode. Call StopKeyEvents to stop.
func (ti *TermInput) StartKeyEvents() <-chan KeyEvent {
	ch := make(chan KeyEvent, 8)
	ti.stopCh = make(chan struct{})
	ti.doneCh = make(chan struct{})
	stop := ti.stopCh
	done := ti.doneCh

	go func() {
		defer close(done)
		defer close(ch)
		for {
			select {
			case <-stop:
				return
			case b, ok := <-ti.byteCh:
				if !ok {
					return
				}
				key := ti.decodeKey(b, stop)
				if key.Rune == 0 && key.Key == "" {
					continue
				}
				select {
				case ch <- key:
				case <-stop:
					return
				}
			}
		}
	}()

	return ch
}

// StopKeyEvents stops the key event goroutine started by StartKeyEvents
// and waits for it to finish, preventing competing goroutines on byteCh.
func (ti *TermInput) StopKeyEvents() {
	if ti.stopCh != nil {
		close(ti.stopCh)
		if ti.doneCh != nil {
			<-ti.doneCh
		}
		ti.stopCh = nil
		ti.doneCh = nil
	}
}

func (ti *TermInput) decodeKey(b byte, stopCh <-chan struct{}) KeyEvent {
	switch {
	case b == 0x1b:
		return ti.decodeEscapeKey(stopCh)
	case b == 0x03:
		return KeyEvent{Key: "ctrl-c"}
	case b == 0x04:
		return KeyEvent{Key: "ctrl-d"}
	case b == '\r' || b == '\n':
		return KeyEvent{Key: "enter"}
	case b == '\t':
		return KeyEvent{Key: "tab"}
	case b == 0x7f || b == 0x08:
		return KeyEvent{Key: "backspace"}
	case b >= 0x20 && b < 0x7f:
		return KeyEvent{Rune: rune(b)}
	default:
		return KeyEvent{}
	}
}

func (ti *TermInput) decodeEscapeKey(stopCh <-chan struct{}) KeyEvent {
	select {
	case <-stopCh:
		return KeyEvent{}
	case b2 := <-ti.byteCh:
		if b2 == '[' {
			select {
			case <-stopCh:
				return KeyEvent{}
			case b3 := <-ti.byteCh:
				switch b3 {
				case 'A':
					return KeyEvent{Key: "up"}
				case 'B':
					return KeyEvent{Key: "down"}
				case 'C':
					return KeyEvent{Key: "right"}
				case 'D':
					return KeyEvent{Key: "left"}
				default:
					if b3 < 0x40 || b3 > 0x7e {
						ti.consumeCSITail(stopCh)
					}
					return KeyEvent{Key: "esc"}
				}
			case <-time.After(escapeTimeout):
				return KeyEvent{Key: "esc"}
			}
		}
		return KeyEvent{Key: "esc"}
	case <-time.After(escapeTimeout):
		return KeyEvent{Key: "esc"}
	}
}

func (ti *TermInput) consumeCSITail(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case b := <-ti.byteCh:
			if b >= 0x40 && b <= 0x7e {
				return
			}
		case <-time.After(escapeTimeout):
			return
		}
	}
}
