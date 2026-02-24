package bench

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// RunAssertions evaluates assertions and returns (passed, failed) counts.
func RunAssertions(assertions []Assertion, artifactsDir, runDir string) (int, int) {
	passed, failed := 0, 0
	for _, a := range assertions {
		if checkAssertion(a, artifactsDir, runDir) {
			passed++
		} else {
			failed++
		}
	}
	return passed, failed
}

func checkAssertion(a Assertion, artifactsDir, runDir string) bool {
	switch a.Type {
	case "file_exists":
		return checkFileExists(a, artifactsDir)
	case "build_passes":
		return checkBuildPasses(a, artifactsDir, runDir)
	case "pattern_match":
		return checkPatternMatch(a, artifactsDir)
	case "file_count_min":
		return checkFileCountMin(a, artifactsDir)
	case "test_passes":
		return checkTestPasses(a, artifactsDir, runDir)
	default:
		return false
	}
}

func checkFileExists(a Assertion, artifactsDir string) bool {
	pattern := filepath.Join(artifactsDir, a.Path)
	matches, err := filepath.Glob(pattern)
	if err == nil && len(matches) > 0 {
		return true
	}
	// Artifacts may be nested under agent subdirectories.
	return recursiveGlob(artifactsDir, a.Path)
}

func checkBuildPasses(a Assertion, artifactsDir, runDir string) bool {
	if a.Command == "" {
		return true
	}
	cmd := strings.ReplaceAll(a.Command, "$SCRATCH_DIR", filepath.Join(runDir, "scratch"))
	cmd = strings.ReplaceAll(cmd, "$ARTIFACTS_DIR", artifactsDir)

	c := exec.Command("sh", "-c", cmd)
	c.Dir = artifactsDir
	c.Stdout = nil
	c.Stderr = nil
	return c.Run() == nil
}

func checkPatternMatch(a Assertion, artifactsDir string) bool {
	if a.Pattern == "" {
		return true
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return false
	}

	pattern := filepath.Join(artifactsDir, a.Path)
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		matches = recursiveGlobFiles(artifactsDir, a.Path)
	}

	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if re.Match(data) {
			return true
		}
	}
	return false
}

func checkFileCountMin(a Assertion, artifactsDir string) bool {
	pattern := filepath.Join(artifactsDir, a.Path)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false
	}
	if len(matches) == 0 {
		matches = recursiveGlobFiles(artifactsDir, a.Path)
	}
	return len(matches) >= a.Min
}

// recursiveGlob checks if any file in the tree matches the pattern,
// supporting both simple filenames and path suffixes.
func recursiveGlob(root, pattern string) bool {
	hasDir := strings.Contains(pattern, string(filepath.Separator)) ||
		strings.Contains(pattern, "/")
	found := false
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if hasDir {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil && matchPathSuffix(rel, pattern) {
				found = true
			}
		} else {
			matched, _ := filepath.Match(pattern, info.Name())
			if matched {
				found = true
			}
		}
		return nil
	})
	return found
}

func checkTestPasses(a Assertion, artifactsDir, runDir string) bool {
	if a.Command == "" {
		return false
	}
	cmd := strings.ReplaceAll(a.Command, "$SCRATCH_DIR", filepath.Join(runDir, "scratch"))
	cmd = strings.ReplaceAll(cmd, "$ARTIFACTS_DIR", artifactsDir)

	c := exec.Command("sh", "-c", cmd)
	c.Dir = artifactsDir
	c.Stdout = nil
	c.Stderr = nil
	return c.Run() == nil
}

func recursiveGlobFiles(root, pattern string) []string {
	hasDir := strings.Contains(pattern, string(filepath.Separator)) ||
		strings.Contains(pattern, "/")
	var results []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if hasDir {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil && matchPathSuffix(rel, pattern) {
				results = append(results, path)
			}
		} else {
			matched, _ := filepath.Match(pattern, info.Name())
			if matched {
				results = append(results, path)
			}
		}
		return nil
	})
	return results
}

func matchPathSuffix(rel, suffix string) bool {
	rel = filepath.ToSlash(rel)
	suffix = filepath.ToSlash(suffix)
	if rel == suffix {
		return true
	}
	return strings.HasSuffix(rel, "/"+suffix)
}
