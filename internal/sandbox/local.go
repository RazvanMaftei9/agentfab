package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Config holds sandboxing settings for local mode.
type Config struct {
	WorkDir        string
	Timeout        time.Duration
	AllowedDirs    []string // filesystem paths the command may access
	KeepBackground bool     // skip process group kill after command exits
	Restrict       *Policy  // OS-level filesystem restriction; nil = no restriction
}

// Run executes a command in a sandboxed environment.
// The env parameter appends extra variables to the stripped base environment.
// Returns (output, pgid, error). When KeepBackground is true, pgid is the
// process group ID that was NOT killed — the caller must eventually call
// KillProcessGroup(pgid). When false (default), pgid is 0.
func Run(ctx context.Context, cfg Config, env []string, name string, args ...string) ([]byte, int, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// Apply OS-level filesystem restrictions if configured.
	if cfg.Restrict != nil {
		name, args = restrict(cfg, *cfg.Restrict, name, args)
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cfg.WorkDir
	// Run in its own process group so we can kill all children on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the entire process group, not just the parent shell.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Ensure work directory exists.
	if err := os.MkdirAll(cfg.WorkDir, 0755); err != nil {
		return nil, 0, fmt.Errorf("create work dir: %w", err)
	}

	// Local-mode environment: inherit the full host environment so agents
	// can use installed tools (node, npm, go, python, etc.) with all their
	// required env vars (GOROOT, NVM_DIR, USER, etc.).
	// Docker/K8s modes provide real isolation and build their own env.
	tmpDir := sandboxTempDir()
	os.MkdirAll(tmpDir, 0755)

	cmdEnv := inheritHostEnv(map[string]string{
		"HOME":   cfg.WorkDir,
		"TMPDIR": tmpDir,
		"LANG":   "en_US.UTF-8",
	})

	// Export allowed directories as indexed env vars (informational for local mode;
	// true filesystem isolation comes in Docker/K8s phases).
	for i, dir := range cfg.AllowedDirs {
		cmdEnv = append(cmdEnv, fmt.Sprintf("SANDBOX_ALLOWED_DIR_%d=%s", i, dir))
	}

	// Append caller-provided extra env vars (can override anything above).
	cmdEnv = setEnvAll(cmdEnv, env...)

	cmd.Env = cmdEnv

	// Capture output to a temp file instead of pipes. cmd.Wait() returns
	// when the shell exits. CombinedOutput() would block until ALL holders
	// of the pipe fd close — including backgrounded children like HTTP
	// servers — causing hangs even when the shell itself exits promptly.
	outFile, err := os.CreateTemp(cfg.WorkDir, ".sandbox-out-*")
	if err != nil {
		return nil, 0, fmt.Errorf("create output file: %w", err)
	}
	outPath := outFile.Name()
	defer os.Remove(outPath)

	cmd.Stdout = outFile
	cmd.Stderr = outFile

	if err := cmd.Start(); err != nil {
		outFile.Close()
		return nil, 0, fmt.Errorf("start command: %w", err)
	}

	// Wait for the shell to exit. Background children may still be running
	// (and writing to outFile) but Wait() returns immediately once the
	// shell process itself exits — no pipe-fd hang.
	waitErr := cmd.Wait()
	outFile.Close()

	output, _ := os.ReadFile(outPath)

	if ctx.Err() == context.DeadlineExceeded {
		return output, 0, fmt.Errorf("command timed out after %s", cfg.Timeout)
	}

	pgid := 0
	if cmd.Process != nil {
		pgid = cmd.Process.Pid
	}

	if cfg.KeepBackground && pgid > 0 {
		// Caller takes ownership of the process group and must call
		// KillProcessGroup(pgid) when done (e.g., after the tool loop).
	} else if pgid > 0 {
		// Default: kill the entire process group.
		// This catches backgrounded children that the shell orphaned.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		pgid = 0
	}

	return output, pgid, waitErr
}

// KillProcessGroup sends SIGKILL to the given process group.
// Safe to call with pgid <= 0 (no-op).
func KillProcessGroup(pgid int) {
	if pgid > 0 {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

// allowedEnvPrefixes are environment variable prefixes safe to inherit.
// Secret-shaped vars (API keys, tokens, credentials) are excluded.
var allowedEnvPrefixes = []string{
	"PATH", "SHELL", "USER", "LOGNAME", "TERM",
	"GOROOT", "GOPATH", "GOBIN", "GOPROXY", "GONOSUMCHECK",
	"NVM_DIR", "NODE_PATH",
	"PYTHON", "VIRTUAL_ENV", "CONDA",
	"JAVA_HOME", "ANDROID_HOME",
	"EDITOR", "VISUAL", "PAGER",
	"LC_", "LANG",
	"XDG_",
	"SSH_AUTH_SOCK",
}

// blockedEnvSubstrings are substrings that indicate a secret.
var blockedEnvSubstrings = []string{
	"KEY", "SECRET", "TOKEN", "PASSWORD", "CREDENTIAL",
	"AUTH", "PRIVATE",
}

// inheritHostEnv copies safe environment variables from the host and applies
// overrides. Only variables matching allowedEnvPrefixes (and not containing
// blockedEnvSubstrings) are inherited. This prevents leaking API keys,
// tokens, and other secrets to sandboxed agent processes.
func inheritHostEnv(overrides map[string]string) []string {
	host := os.Environ()
	result := make([]string, 0, len(overrides)+32)

	overrideKeys := make(map[string]bool, len(overrides))
	for k := range overrides {
		overrideKeys[strings.ToUpper(k)] = true
	}

	for _, entry := range host {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := entry[:eq]
		upper := strings.ToUpper(key)

		if overrideKeys[upper] {
			continue
		}
		if !isAllowedEnv(upper) {
			continue
		}
		result = append(result, entry)
	}

	// Apply overrides.
	for k, v := range overrides {
		result = append(result, k+"="+v)
	}
	return result
}

// isAllowedEnv returns true if the variable name matches the whitelist
// and does not contain any blocked substrings.
func isAllowedEnv(upper string) bool {
	allowed := false
	for _, prefix := range allowedEnvPrefixes {
		if strings.HasPrefix(upper, prefix) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	// Reject even whitelisted prefixes if they look secret-shaped.
	// e.g., GOPATH is fine but GO_SECRET_KEY is not.
	for _, blocked := range blockedEnvSubstrings {
		if strings.Contains(upper, blocked) {
			return false
		}
	}
	return true
}

// setEnvAll applies env entries (KEY=VALUE) to an existing env slice,
// replacing existing keys or appending new ones.
func setEnvAll(base []string, entries ...string) []string {
	for _, entry := range entries {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key := entry[:eq]
		found := false
		for i, existing := range base {
			if eqE := strings.IndexByte(existing, '='); eqE > 0 {
				if strings.EqualFold(existing[:eqE], key) {
					base[i] = entry
					found = true
					break
				}
			}
		}
		if !found {
			base = append(base, entry)
		}
	}
	return base
}
