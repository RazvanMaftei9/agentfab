package ui

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestStdinPollerModeSwitchRace verifies that Go's non-blocking I/O poller
// correctly delivers bytes after a terminal mode switch (cooked → raw).
// This is the suspected root cause of keystroke loss on macOS.
func TestStdinPollerModeSwitchRace(t *testing.T) {
	if os.Getenv("AGENTFAB_TTY_TEST") == "" {
		t.Skip("set AGENTFAB_TTY_TEST=1 and run from a TTY to test")
	}

	fd := int(os.Stdin.Fd())

	// Check if stdin is a tty.
	if !IsTTY(os.Stdin) {
		t.Skip("stdin is not a tty")
	}

	// Verify Go set O_NONBLOCK on stdin.
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("fcntl F_GETFL: %v", err)
	}
	nonblock := flags&unix.O_NONBLOCK != 0
	t.Logf("stdin fd=%d, O_NONBLOCK=%v", fd, nonblock)

	// Verify /dev/tty can be opened independently.
	ttyFd, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open /dev/tty: %v", err)
	}
	defer unix.Close(ttyFd)

	ttyFlags, _ := unix.FcntlInt(uintptr(ttyFd), unix.F_GETFL, 0)
	t.Logf("/dev/tty fd=%d, O_NONBLOCK=%v", ttyFd, ttyFlags&unix.O_NONBLOCK != 0)

	// Verify blocking read on /dev/tty works in raw mode.
	if err := enterCbreak(fd); err != nil {
		t.Fatalf("enterCbreak: %v", err)
	}
	defer func() {
		state, _ := unix.IoctlGetTermios(fd, termIoctlGet)
		state.Lflag |= unix.ECHO | unix.ICANON
		unix.IoctlSetTermios(fd, termIoctlSet, state)
	}()

	fmt.Print("Press any key within 3 seconds: ")
	done := make(chan bool, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		buf := make([]byte, 256)
		n, err := unix.Read(ttyFd, buf[:])
		if err != nil {
			t.Logf("unix.Read error: %v", err)
			done <- false
			return
		}
		t.Logf("unix.Read: %d bytes: %v", n, buf[:n])
		done <- true
	}()

	select {
	case ok := <-done:
		if ok {
			t.Log("PASS: blocking read on /dev/tty works")
		} else {
			t.Error("blocking read on /dev/tty failed")
		}
	case <-time.After(3 * time.Second):
		t.Log("Timeout (expected if no key was pressed)")
	}
}
