package main

import (
	"os"
	"path/filepath"
)

// hooksPaused applies the installation pause to hook bookkeeping only. Ordinary
// CLI commands remain available while the hooks are paused.
func hooksPaused() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(home, ".config", "one-shot-tally", "disabled"))
	return err == nil && !info.IsDir()
}
