//go:build linux

package ui

import "golang.org/x/sys/unix"

const (
	termIoctlGet      = unix.TCGETS
	termIoctlSet      = unix.TCSETS
	termIoctlSetFlush = unix.TCSETSF // TCSAFLUSH: apply after flushing input queue
)
