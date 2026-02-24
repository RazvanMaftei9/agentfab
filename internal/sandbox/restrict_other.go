//go:build !darwin && !linux

package sandbox

import "log/slog"

// restrict is a no-op on platforms without OS-level sandbox support.
// A warning is logged on first invocation.
func restrict(cfg Config, policy Policy, name string, args []string) (string, []string) {
	slog.Warn("OS-level filesystem sandbox not available on this platform")
	return name, args
}
