package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/razvanmaftei/agentfab/internal/ui"
	"github.com/razvanmaftei/agentfab/internal/version"
	"github.com/spf13/cobra"
)

type providerInfo struct {
	Name   string
	EnvVar string
}

var providers = []providerInfo{
	{"Anthropic", "ANTHROPIC_API_KEY"},
	{"OpenAI", "OPENAI_API_KEY"},
	{"Google", "GOOGLE_API_KEY"},
	{"OpenAI-compatible", "OPENAI_COMPAT_API_KEY"},
}

func setupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Interactive setup: configure API keys and install agentfab globally",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := os.Stdout
			tty := ui.IsTTY(os.Stdin)

			if tty {
				fmt.Fprint(w, ui.ClearScreen)
			}

			drawSetupHeader(w, tty)

			var ti *ui.TermInput
			if tty {
				ti = ui.NewTermInput()
				defer ti.Close()
			}

			if err := setupAPIKeys(w, ti, tty); err != nil {
				return err
			}

			if err := setupInstall(w, ti, tty); err != nil {
				return err
			}

			fmt.Fprintln(w)
			fmt.Fprintf(w, "  %s%s Setup complete.%s\n", ui.Green, ui.Bold, ui.Reset)
			fmt.Fprintf(w, "  Run %sagentfab init%s in a project directory to get started.\n\n", ui.Bold, ui.Reset)
			return nil
		},
	}
	return cmd
}

func drawSetupHeader(w io.Writer, tty bool) {
	if !tty {
		fmt.Fprintf(w, "agentfab v%s setup\n\n", version.Version)
		return
	}
	fmt.Fprintf(w, "\n  %s%sagentfab%s %ssetup%s  %sv%s%s\n",
		ui.Bold, ui.White, ui.Reset,
		ui.Dim, ui.Reset,
		ui.Gray, version.Version, ui.Reset)
	fmt.Fprintf(w, "  %s%s%s\n", ui.Gray, strings.Repeat("─", 36), ui.Reset)
}

func setupAPIKeys(w io.Writer, ti *ui.TermInput, tty bool) error {
	fmt.Fprintf(w, "\n  %s%sAPI Keys%s\n\n", ui.Bold, ui.Teal, ui.Reset)

	rcFile := detectShellRC()
	persisted := false

	for i, p := range providers {
		if val := os.Getenv(p.EnvVar); val != "" {
			fmt.Fprintf(w, "  %s%s %s%s %s(%s)%s\n",
				ui.Green, checkMark, ui.Reset, p.Name, ui.Dim, p.EnvVar, ui.Reset)
			if i < len(providers)-1 {
				fmt.Fprintln(w)
			}
			continue
		}

		prompt := fmt.Sprintf("  %s%s%s API key %s(Enter to skip)%s: ",
			ui.Bold, p.Name, ui.Reset, ui.Dim, ui.Reset)

		var key string
		if ti != nil {
			line, ok := ti.ReadLine(w, prompt)
			if !ok {
				return fmt.Errorf("interrupted")
			}
			key = strings.TrimSpace(line)
		} else {
			fmt.Fprint(w, prompt)
			var line string
			fmt.Scanln(&line)
			key = strings.TrimSpace(line)
		}

		if key == "" {
			fmt.Fprintf(w, "  %s%s skipped%s\n", ui.Dim, dashMark, ui.Reset)
			if i < len(providers)-1 {
				fmt.Fprintln(w)
			}
			continue
		}

		exportLine := fmt.Sprintf("export %s=%q", p.EnvVar, key)

		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %sAdd to %s:%s\n", ui.Dim, rcFile, ui.Reset)
		fmt.Fprintf(w, "  %s%s%s\n", ui.Gray, exportLine, ui.Reset)
		fmt.Fprintln(w)

		choice := setupPickPersist(w, ti, tty, rcFile)

		if choice == 0 {
			if err := appendToFile(rcFile, exportLine); err != nil {
				return fmt.Errorf("writing to %s: %w", rcFile, err)
			}
			fmt.Fprintf(w, "  %s%s Appended to %s%s\n", ui.Green, checkMark, rcFile, ui.Reset)
			persisted = true
		} else {
			fmt.Fprintf(w, "  %s%s Manual — add it when you're ready%s\n", ui.Dim, dashMark, ui.Reset)
		}

		if i < len(providers)-1 {
			fmt.Fprintln(w)
		}
	}

	if persisted {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %sRun %ssource %s%s to load the new keys.%s\n",
			ui.Dim, ui.Bold+ui.Reset, rcFile, ui.Dim, ui.Reset)
	}

	return nil
}

func setupPickPersist(w io.Writer, ti *ui.TermInput, tty bool, rcFile string) int {
	options := []string{
		"Persist (append to " + rcFile + ")",
		"I'll do this manually",
	}

	if tty && ti != nil {
		return arrowSelect(w, ti, options)
	}

	for i, opt := range options {
		fmt.Fprintf(w, "  [%d] %s\n", i+1, opt)
	}
	fmt.Fprint(w, "  Choice [1]: ")
	var line string
	fmt.Scanln(&line)
	line = strings.TrimSpace(line)
	if line == "2" {
		return 1
	}
	return 0
}

func setupInstall(w io.Writer, ti *ui.TermInput, tty bool) error {
	fmt.Fprintf(w, "\n\n  %s%sInstall%s\n\n", ui.Bold, ui.Teal, ui.Reset)

	repoRoot, err := findRepoRoot()
	if err != nil {
		return fmt.Errorf("cannot find project root (Makefile): %w", err)
	}

	spinner := ui.NewSpinner(w, tty)
	spinner.Start("Building agentfab...")

	buildCmd := exec.Command("make", "build")
	buildCmd.Dir = repoRoot
	if err := buildCmd.Run(); err != nil {
		spinner.Stop()
		return fmt.Errorf("build failed: %w", err)
	}
	spinner.Stop()
	fmt.Fprintf(w, "  %s%s Build complete%s\n\n", ui.Green, checkMark, ui.Reset)

	builtBinary := filepath.Join(repoRoot, "agentfab")
	installDir, needsSudo := pickInstallDir()
	dest := filepath.Join(installDir, "agentfab")

	spinner = ui.NewSpinner(w, tty)
	spinner.Start(fmt.Sprintf("Installing to %s...", dest))

	if err := installBinary(builtBinary, dest, needsSudo); err != nil {
		spinner.Stop()
		return fmt.Errorf("install failed: %w", err)
	}
	spinner.Stop()

	if which, err := exec.LookPath("agentfab"); err == nil {
		fmt.Fprintf(w, "  %s%s agentfab v%s installed at %s%s\n",
			ui.Green, checkMark, version.Version, which, ui.Reset)
	} else {
		fmt.Fprintf(w, "  %s%s Installed to %s%s\n", ui.Green, checkMark, dest, ui.Reset)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  %s%s is not in your PATH. Add it with:%s\n", ui.Yellow, installDir, ui.Reset)
		fmt.Fprintf(w, "  %sexport PATH=\"%s:$PATH\"%s\n", ui.Gray, installDir, ui.Reset)
	}

	return nil
}

const dashMark = "–"

func arrowSelect(w io.Writer, ti *ui.TermInput, options []string) int {
	selected := 0

	if err := ti.EnterRaw(); err != nil {
		return 0
	}
	keyCh := ti.StartKeyEvents()

	lines := drawArrowSelect(w, options, selected)

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
				lines = drawArrowSelect(w, options, selected)
			}
		case key.Key == "down":
			if selected < len(options)-1 {
				selected++
				eraseLines(w, lines)
				lines = drawArrowSelect(w, options, selected)
			}
		case key.Key == "enter":
			ti.StopKeyEvents()
			ti.Drain()
			ti.ExitRaw()
			eraseLines(w, lines)
			return selected
		case key.Rune >= '1' && key.Rune <= '9':
			idx := int(key.Rune - '1')
			if idx < len(options) {
				ti.StopKeyEvents()
				ti.Drain()
				ti.ExitRaw()
				eraseLines(w, lines)
				return idx
			}
		}
	}

	ti.ExitRaw()
	return selected
}

func drawArrowSelect(w io.Writer, options []string, selected int) int {
	lines := 0
	for i, opt := range options {
		if i == selected {
			fmt.Fprintf(w, "  %s▸ %s%s%s\n", ui.Cyan, ui.Bold, opt, ui.Reset)
		} else {
			fmt.Fprintf(w, "    %s%s%s\n", ui.Dim, opt, ui.Reset)
		}
		lines++
	}
	fmt.Fprintf(w, "  %s↑↓%s%s navigate  %sEnter%s%s select%s\n",
		ui.Bold, ui.Reset, ui.Dim, ui.Bold, ui.Reset, ui.Dim, ui.Reset)
	lines++
	return lines
}

func eraseLines(w io.Writer, n int) {
	if n > 0 {
		fmt.Fprint(w, strings.Repeat(ui.MoveUp+ui.ClearLn, n))
	}
}

func installBinary(src, dest string, useSudo bool) error {
	if useSudo {
		cmd := exec.Command("sudo", "install", "-m", "755", src, dest)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("install", "-m", "755", src, dest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func pickInstallDir() (dir string, needsSudo bool) {
	if runtime.GOOS == "windows" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "bin"), false
	}

	target := "/usr/local/bin"
	if isWritable(target) {
		return target, false
	}
	if _, err := os.Stat(target); err == nil {
		return target, true
	}

	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin"), false
}

func isWritable(dir string) bool {
	tmp := filepath.Join(dir, ".agentfab_write_test")
	f, err := os.Create(tmp)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(tmp)
	return true
}

func detectShellRC() string {
	shell := os.Getenv("SHELL")
	home, _ := os.UserHomeDir()

	switch {
	case strings.Contains(shell, "zsh"):
		return filepath.Join(home, ".zshrc")
	case strings.Contains(shell, "bash"):
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	case strings.Contains(shell, "fish"):
		return filepath.Join(home, ".config", "fish", "config.fish")
	default:
		return filepath.Join(home, ".profile")
	}
}

func appendToFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n%s\n", line)
	return err
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate repo root")
	}
	exeDir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(exeDir, "Makefile")); err == nil {
		return exeDir, nil
	}

	return "", fmt.Errorf("cannot locate repo root — run setup from the agentfab source directory")
}
