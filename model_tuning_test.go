package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentAgentToolNames(t *testing.T) {
	for _, tool := range []string{"Agent", "spawn_agent", "collaboration.spawn_agent", "collaborationspawn_agent", "functions.spawn_agent"} {
		if !isAgentSpawn(tool) {
			t.Errorf("missed spawn tool %q", tool)
		}
	}
	for _, tool := range []string{"collaboration.followup_task", "agent.followup_task", "agent_task_status", "spark_database", "not_spawn_agent"} {
		if isAgentSpawn(tool) {
			t.Errorf("non-spawn tool %q counted as a new agent", tool)
		}
	}
}

func TestModelsAreObservedWithoutRoleOrPriceInference(t *testing.T) {
	dir := retainedTestDir(t)
	for i, role := range []string{"spark_worker", "worker", "explorer", "complex_implementor"} {
		hook(t, dir, map[string]any{
			"session_id": "models", "turn_id": "current", "hook_event_name": "PreToolUse",
			"model": "gpt-6.1-sol", "tool_name": "Agent", "tool_use_id": role,
			"tool_input": map[string]any{"agent_type": role, "task_name": role, "model": []string{"gpt-6-luna", "gpt-6-luna", "gpt-6.1-sol", "gpt-6-astra"}[i]},
		})
	}
	s := loadTestState(t, dir)
	if s.TotalCalls != 4 || s.AgentCalls != 4 || s.CallCostUnits != 16 || s.ModelCalls["gpt-6.1-sol"] != 4 || len(s.ModelCalls) != 1 {
		t.Fatalf("model accounting inferred child usage or discounted roles: %#v", s)
	}
	if !strings.Contains(reportLine(s), "Observed models: gpt-6.1-sol: 4") {
		t.Fatal("report omits the observed runtime model")
	}
}

func TestLegacyDiscountMigration(t *testing.T) {
	dir := retainedTestDir(t)
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	p, err := statePath(event{SessionID: "legacy", TurnID: "discount"})
	if err != nil {
		t.Fatal(err)
	}
	s := emptyState("legacy", "discount")
	s.StateVersion, s.TotalCalls, s.SparkCalls, s.CallCostUnits = 5, 4, 4, 4
	s.TestFingerprints["old"] = 2
	s.RedundantTestMillis = 100
	s.Pending["test"] = pendingCall{RepeatedTest: true}
	s.Running = map[string]pendingCall{"42": {RepeatedTest: true}}
	if err := save(p, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(p, "legacy", "discount")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateVersion != 6 || loaded.CallCostUnits != 16 || loaded.SparkCalls != 4 || len(loaded.TestFingerprints) != 0 || loaded.RedundantTestMillis != 0 || loaded.Pending["test"].RepeatedTest || loaded.Running["42"].RepeatedTest {
		t.Fatalf("legacy state not migrated: %#v", loaded)
	}
}

func TestMissingAndChangedRuntimeModels(t *testing.T) {
	dir := retainedTestDir(t)
	for _, model := range []string{"gpt-6-astra", "", "gpt-6.1-sol", "gpt-6-astra"} {
		hook(t, dir, map[string]any{"session_id": "switch", "turn_id": "models", "hook_event_name": "PreToolUse", "model": model, "tool_name": "Agent", "tool_input": map[string]any{"agent_type": "worker"}})
	}
	s := loadTestState(t, dir)
	if len(s.ModelCalls) != 2 || s.ModelCalls["gpt-6-astra"] != 2 || s.ModelCalls["gpt-6.1-sol"] != 1 || s.CallCostUnits != 16 {
		t.Fatalf("missing or switched model miscounted: %#v", s.ModelCalls)
	}
}

func TestRequiredContinuationsAreNotPassiveWaits(t *testing.T) {
	for _, tool := range []string{"functions.wait", "wait", "collaboration.wait_agent"} {
		if passiveWait(event{ToolName: tool, ToolInput: json.RawMessage(`{"cell_id":"42"}`)}, "") {
			t.Errorf("required continuation %s penalized", tool)
		}
	}
	if !passiveWait(event{ToolName: "Bash"}, "sleep 30") || !passiveWait(event{ToolName: "functions.write_stdin", ToolInput: json.RawMessage(`{"session_id":42}`)}, "") {
		t.Fatal("untracked polling no longer recorded")
	}
}

func TestRepairMakesRerunNewVerification(t *testing.T) {
	dir := retainedTestDir(t)
	pre := func(id, tool string, input any) {
		t.Helper()
		hook(t, dir, map[string]any{"session_id": "repair", "turn_id": "rerun", "hook_event_name": "PreToolUse", "tool_use_id": id, "tool_name": tool, "tool_input": input})
	}
	post := func(id string, code int) {
		t.Helper()
		hook(t, dir, map[string]any{"session_id": "repair", "turn_id": "rerun", "hook_event_name": "PostToolUse", "tool_use_id": id, "tool_name": "Bash", "tool_response": map[string]any{"exit_code": code, "duration_ms": 100}})
	}
	input := map[string]any{"command": "go test ./..."}
	pre("initial", "Bash", input)
	post("initial", 1)
	pre("repair", "apply_patch", map[string]any{})
	pre("rerun", "Bash", input)
	if s := loadTestState(t, dir); s.Pending["rerun"].RepeatedTest {
		t.Fatal("rerun after a repair counted as redundant")
	}
	post("rerun", 0)
	pre("unchanged", "Bash", input)
	if s := loadTestState(t, dir); !s.Pending["unchanged"].RepeatedTest || s.RedundantTestMillis != 0 {
		t.Fatalf("unchanged repetition or repair time misclassified: %#v", s)
	}
}

func TestPausedHookDoesNotReadInputOrWriteState(t *testing.T) {
	home := retainedTestDir(t)
	t.Setenv("HOME", home)
	stateDir := filepath.Join(home, "unused-state")
	t.Setenv("ONE_SHOT_STATE_DIR", stateDir)
	marker := filepath.Join(home, ".config", "one-shot-tally", "disabled")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("paused\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"paused","turn_id":"paused","tool_name":"Bash"}`)
	var output, diagnostics bytes.Buffer
	runHookFailOpen(input, &output, &diagnostics)
	if strings.TrimSpace(output.String()) != "{}" || diagnostics.Len() != 0 || input.Len() == 0 {
		t.Fatalf("paused hook did work: output=%q diagnostics=%q", output.String(), diagnostics.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("paused hook wrote state: %v", err)
	}
	output.Reset()
	printVersion(&output)
	if !strings.Contains(output.String(), binaryVersion) {
		t.Fatal("pause disabled CLI diagnostics")
	}
	// Saved reports remain readable through the same status command dispatcher
	// uses, without clearing the pause or running hook bookkeeping.
	p, err := statePath(event{SessionID: "saved", TurnID: "saved"})
	if err != nil {
		t.Fatal(err)
	}
	if err := save(p, emptyState("saved", "saved")); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := printLatestTo(&output, true); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report["hooks_paused"] != true {
		t.Fatalf("paused status unavailable: %s (%v)", output.String(), err)
	}
}
