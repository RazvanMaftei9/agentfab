//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package ui

import "golang.org/x/sys/unix"

const (
	termIoctlGet      = unix.TIOCGETA
	termIoctlSet      = unix.TIOCSETA
	termIoctlSetFlush = unix.TIOCSETAF // TCSAFLUSH: apply after flushing input queue
)
