//go:build linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const landlockSentinel = "__agentfab_landlock__"

func init() {
	// Self-reexec check: if this binary was invoked with the Landlock
	// sentinel as argv[1], apply the policy and exec the real command.
	// This runs before main/TestMain, replacing the process entirely.
	if len(os.Args) >= 4 && os.Args[1] == landlockSentinel {
		policyFile := os.Args[2]
		realCmd := os.Args[3]
		realArgs := os.Args[3:] // argv[0] = realCmd

		data, err := os.ReadFile(policyFile)
		os.Remove(policyFile) // clean up
		if err != nil {
			fmt.Fprintf(os.Stderr, "landlock-exec: read policy: %v\n", err)
			os.Exit(126)
		}

		var policy Policy
		if err := json.Unmarshal(data, &policy); err != nil {
			fmt.Fprintf(os.Stderr, "landlock-exec: parse policy: %v\n", err)
			os.Exit(126)
		}

		if err := EnforceLandlock(policy); err != nil {
			fmt.Fprintf(os.Stderr, "landlock-exec: enforce: %v\n", err)
			os.Exit(126)
		}

		// Replace this process with the real command, now under Landlock.
		bin, err := exec.LookPath(realCmd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "landlock-exec: lookup %q: %v\n", realCmd, err)
			os.Exit(127)
		}
		if err := syscall.Exec(bin, realArgs, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "landlock-exec: exec %q: %v\n", bin, err)
			os.Exit(126)
		}
	}
}

// restrict wraps a command with Landlock filesystem restrictions by
// re-executing the current binary with a sentinel arg. The re-invoked
// process applies Landlock to itself and then execs the real command.
// On kernels < 5.13 it logs a warning and returns unchanged.
func restrict(cfg Config, policy Policy, name string, args []string) (string, []string) {
	if !landlockAvailable() {
		slog.Warn("Landlock not available on this kernel, filesystem sandbox disabled")
		return name, args
	}

	// Serialize policy to a temp file.
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		slog.Error("failed to serialize Landlock policy", "error", err)
		return name, args
	}

	f, err := os.CreateTemp("", "agentfab-landlock-*.json")
	if err != nil {
		slog.Error("failed to create Landlock policy file", "error", err)
		return name, args
	}
	if _, err := f.Write(policyJSON); err != nil {
		f.Close()
		os.Remove(f.Name())
		slog.Error("failed to write Landlock policy", "error", err)
		return name, args
	}
	policyPath := f.Name()
	f.Close()

	// Re-invoke ourselves with the sentinel. The init() above will
	// apply Landlock and exec the real command.
	self, err := os.Executable()
	if err != nil {
		os.Remove(policyPath)
		slog.Error("failed to find own executable for Landlock wrapper", "error", err)
		return name, args
	}

	// Resolve symlinks so /proc/self/exe works correctly.
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	newArgs := make([]string, 0, len(args)+3)
	newArgs = append(newArgs, landlockSentinel, policyPath, name)
	newArgs = append(newArgs, args...)

	slog.Debug("Landlock sandbox: wrapping command via self-reexec",
		"read_write", policy.ReadWrite,
		"read_only", policy.ReadOnly,
	)

	return self, newArgs
}

// landlockAvailable checks if the kernel supports Landlock by attempting
// to create a ruleset with a minimal set of handled accesses.
func landlockAvailable() bool {
	attr := unix.LandlockRulesetAttr{
		Access_fs: unix.LANDLOCK_ACCESS_FS_READ_FILE |
			unix.LANDLOCK_ACCESS_FS_WRITE_FILE,
	}
	fd, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return false
	}
	unix.Close(int(fd))
	return true
}

// EnforceLandlock applies Landlock restrictions to the current process.
// Exported for use by helper binaries or test harnesses.
// Must be called before exec in the child process.
func EnforceLandlock(policy Policy) error {
	attr := unix.LandlockRulesetAttr{
		Access_fs: unix.LANDLOCK_ACCESS_FS_EXECUTE |
			unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_FILE |
			unix.LANDLOCK_ACCESS_FS_READ_DIR |
			unix.LANDLOCK_ACCESS_FS_MAKE_REG |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
			unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
	}

	fd, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attr)),
		unsafe.Sizeof(attr),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	rulesetFd := int(fd)
	defer unix.Close(rulesetFd)

	rwAccess := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)

	roAccess := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR)

	// Add read-write rules.
	for _, p := range policy.ReadWrite {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if err := addLandlockPath(rulesetFd, abs, rwAccess); err != nil {
			return fmt.Errorf("add rw rule for %q: %w", abs, err)
		}
	}

	// Add read-only rules.
	for _, p := range policy.ReadOnly {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if err := addLandlockPath(rulesetFd, abs, roAccess); err != nil {
			return fmt.Errorf("add ro rule for %q: %w", abs, err)
		}
	}

	// Allow reading system paths (executables, libraries).
	systemAccess := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_EXECUTE)

	systemReadPaths := []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/proc"}
	for _, sp := range systemReadPaths {
		if _, err := os.Stat(sp); err != nil {
			continue
		}
		_ = addLandlockPath(rulesetFd, sp, systemAccess)
	}

	// /dev needs write access too — tools write to /dev/null, /dev/tty, etc.
	devAccess := systemAccess | unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	if _, err := os.Stat("/dev"); err == nil {
		_ = addLandlockPath(rulesetFd, "/dev", devAccess)
	}

	// Allow read-only access to detected toolchain paths.
	for _, tp := range detectToolchainPaths() {
		_ = addLandlockPath(rulesetFd, tp, roAccess)
	}

	// Allow temp directory access.
	tmpDir := os.TempDir()
	_ = addLandlockPath(rulesetFd, tmpDir, rwAccess)

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl PR_SET_NO_NEW_PRIVS: %w", err)
	}

	_, _, errno = unix.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(rulesetFd),
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", errno)
	}

	return nil
}

func addLandlockPath(rulesetFd int, path string, access uint64) error {
	pathFd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(pathFd)

	attr := unix.LandlockPathBeneathAttr{
		Allowed_access: access,
		Parent_fd:      int32(pathFd),
	}

	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFd),
		uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
		uintptr(unsafe.Pointer(&attr)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
