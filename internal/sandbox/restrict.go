package sandbox

// Policy defines OS-level filesystem access restrictions for sandboxed commands.
// When set on Config.Restrict, the sandbox wraps commands with platform-specific
// mechanisms (macOS sandbox-exec, Linux Landlock) to enforce read/write boundaries.
type Policy struct {
	ReadOnly  []string // paths with read-only access
	ReadWrite []string // paths with read-write access
}
