package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHooksPaused(t *testing.T) {
	for _, scenario := range []string{"absent", "present", "directory", "stat error", "home unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			marker := filepath.Join(home, ".config", "one-shot-tally", "disabled")
			switch scenario {
			case "present", "directory":
				if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
					t.Fatal(err)
				}
				if scenario == "directory" {
					if err := os.Mkdir(marker, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(marker, []byte("paused\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "stat error":
				// A regular file in the parent path reliably produces ENOTDIR,
				// including when tests run as a privileged user.
				if err := os.WriteFile(filepath.Join(home, ".config"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "home unavailable":
				t.Setenv("HOME", "")
			}
			if got, want := hooksPaused(), scenario == "present"; got != want {
				t.Fatalf("hooksPaused() = %v, want %v", got, want)
			}
		})
	}
}

func TestCodexHookTemplate(t *testing.T) {
	data, err := os.ReadFile("hooks/codex.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Hooks) != 5 {
		t.Fatalf("unexpected events: %#v", config.Hooks)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"} {
		groups := config.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("missing or duplicate handler for %s", event)
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" || h.Command != `"$HOME/.local/bin/one-shot-tally"` || h.Timeout <= 0 || h.Timeout > 5 {
			t.Fatalf("unexpected handler for %s: %#v", event, h)
		}
	}
}
