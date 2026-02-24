package sandbox

import (
	"os"
	"path/filepath"
)

func sandboxTempDir() string {
	return filepath.Join(os.TempDir(), "agentfab-sandbox")
}
