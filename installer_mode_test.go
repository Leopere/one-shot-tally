package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTallyOnlyInstallerMode(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, want := range []string{
		"[ \"$1\" = \"--tally-only\" ]",
		"install_mode=tally-only",
		"one-shot-tally 1.23.0 | ColinKnapp.com",
		"if [ \"$install_mode\" = tally-only ]; then",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install.sh is missing %q", want)
		}
	}
	if strings.Index(script, "if [ \"$install_mode\" = tally-only ]; then") > strings.Index(script, "agent-file-guard") {
		t.Fatal("tally-only mode must exit before file-guard installation")
	}
	if output, err := exec.Command("sh", "-n", filepath.Clean("install.sh")).CombinedOutput(); err != nil {
		t.Fatalf("install.sh syntax: %v\n%s", err, output)
	}
}
