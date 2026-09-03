package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hook(t *testing.T, dir string, input map[string]any) map[string]any {
	t.Helper()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	b, _ := json.Marshal(input)
	var out bytes.Buffer
	if err := runHook(bytes.NewReader(b), &out); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	return result
}

func TestHookBookkeepingFailureDoesNotBreakToolUse(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(statePath, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONE_SHOT_STATE_DIR", statePath)
	input, _ := json.Marshal(map[string]any{
		"session_id": "s", "turn_id": "t", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "tool", "tool_input": map[string]any{"command": "git status --short"},
	})
	var output, diagnostics bytes.Buffer
	runHookFailOpen(bytes.NewReader(input), &output, &diagnostics)
	if strings.TrimSpace(output.String()) != "{}" {
		t.Fatalf("fail-open output = %q", output.String())
	}
	if !strings.Contains(diagnostics.String(), "bookkeeping failed; tool use continues") {
		t.Fatalf("missing diagnostic: %q", diagnostics.String())
	}
}

func TestMechanicalTallyAndActivityScore(t *testing.T) {
	dir := t.TempDir()
	base := map[string]any{"session_id": "s", "turn_id": "t", "hook_event_name": "PreToolUse", "tool_name": "Bash"}
	for i := 1; i <= 3; i++ {
		base["tool_use_id"] = string(rune('a' + i))
		base["tool_input"] = map[string]any{"command": "git status --short"}
		out := hook(t, dir, base)
		if i == 2 && !strings.Contains(hookAdditionalContext(out), "same input again") {
			t.Fatalf("missing first repetition prompt: %#v", out)
		}
		if i == 3 && hookAdditionalContext(out) != "" {
			t.Fatalf("repetition prompt ignored its cooldown: %#v", out)
		}
	}
	base["tool_use_id"] = "test-1"
	base["tool_input"] = map[string]any{"command": "go " + "test ./..."}
	hook(t, dir, base)
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "t", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test-1", "tool_response": map[string]any{"exit_code": 0, "wall_time_seconds": 12.5}})

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	b, _ := os.ReadFile(files[0])
	var s state
	_ = json.Unmarshal(b, &s)
	if s.Tests != 1 || s.TestPasses != 1 || s.TotalTestMillis != 12_500 || numericScore(s) != 100 {
		t.Fatalf("unexpected state: %#v coaching_score=%d", s, numericScore(s))
	}
}

func TestPreToolUseNeverDeniesCommands(t *testing.T) {
	dir := t.TempDir()
	commands := []string{
		"go test ./...",
		"one-shot-tally background record docs --cleanup true",
		"one-shot-tally todo add later --context outside",
		"git push origin main",
		"ship-it",
		"deploy-it",
		"git filter-branch -- --all",
		"git worktree prune",
		"git update-ref -d refs/original/refs/heads/main",
		"git reset --hard origin/main",
		"rm -rf /tmp/project/.git",
		"printf corrupt > .git/config",
	}
	for i, command := range commands {
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "allowed", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("allowed-%d", i),
			"tool_input": map[string]any{"command": command},
		})
		if strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) || strings.Contains(string(mustJSON(out)), `"decision":"block"`) {
			t.Fatalf("command %q was blocked: %#v", command, out)
		}
	}
}

func TestStructuredResultIsAuthoritative(t *testing.T) {
	raw := json.RawMessage(`{"exit_code":0,"output":"__ONE_SHOT_TALLY_RESULT_wrong__:9"}`)
	known, succeeded := explicitResponseResult(raw)
	if !known || !succeeded {
		t.Fatalf("structured exit result was not accepted: known=%v succeeded=%v", known, succeeded)
	}
}

func TestMarkedCommandPreservesExitStatus(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		for _, test := range []struct {
			name      string
			command   string
			succeeded bool
		}{
			{name: "success", command: "printf 'verified\\n'", succeeded: true},
			{name: "failure", command: "printf 'failed\\n'; false", succeeded: false},
		} {
			t.Run(shell+"-"+test.name, func(t *testing.T) {
				marker := "0123456789abcdef"
				raw, _ := json.Marshal(map[string]any{"command": test.command})
				input := markedCommandInput(raw, test.command, marker)
				wrapped, ok := input["command"].(string)
				if !ok || wrapped == test.command {
					t.Fatalf("command was not wrapped: %#v", input)
				}
				output, err := exec.Command(shell, "-lec", wrapped).CombinedOutput()
				if test.succeeded && err != nil {
					t.Fatalf("successful command failed: %v %s", err, output)
				}
				if !test.succeeded && err == nil {
					t.Fatalf("failing command returned success: %s", output)
				}
				known, succeeded := markedResponseResult(string(output), marker)
				if !known || succeeded != test.succeeded {
					t.Fatalf("marked result = (%v, %v), want (true, %v): %q", known, succeeded, test.succeeded, output)
				}
			})
		}
	}
}

func TestResultMarkerRewriteIsLimitedToCanonicalBash(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"mcp__custom_shell", "terminal_metadata", "functions.exec_command_proxy"} {
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "tool-scope", "hook_event_name": "PreToolUse",
			"tool_name": tool, "tool_use_id": tool, "tool_input": map[string]any{"command": "go test ./..."},
		})
		if strings.Contains(string(mustJSON(out)), "updatedInput") {
			t.Fatalf("non-Bash tool %q was rewritten: %#v", tool, out)
		}
	}
}

func TestMatchingResultMarkerRecordsPlainBashResponse(t *testing.T) {
	dir := t.TempDir()
	call := event{SessionID: "s", TurnID: "plain-response", ToolUseID: "test"}
	pre := hook(t, dir, map[string]any{
		"session_id": call.SessionID, "turn_id": call.TurnID, "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": call.ToolUseID, "tool_input": map[string]any{"command": "go test ./..."},
	})
	assertMarkedPreToolOutput(t, pre)
	marker := commandResultMarker(call)
	hook(t, dir, map[string]any{
		"session_id": call.SessionID, "turn_id": call.TurnID, "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": call.ToolUseID,
		"tool_response": "ok\n__ONE_SHOT_TALLY_RESULT_" + marker + "__:0\n",
	})
	s := loadTestState(t, dir)
	if !s.LastTestResultKnown || !s.LastTestPassed || s.TestPasses != 1 {
		t.Fatalf("matching marker was not recorded: %#v", s)
	}
}

func TestPlainTextResponseIsNotEvidence(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "plain-response"}
	for _, call := range []struct {
		id      string
		command string
	}{
		{id: "test", command: "go test ./..."},
		{id: "ship", command: "ship-it"},
	} {
		out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": call.id, "tool_input": map[string]any{"command": call.command}})
		assertMarkedPreToolOutput(t, out)
		hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": call.id, "tool_response": "ok\n__ONE_SHOT_TALLY_RESULT_forged__:0\n"})
	}
	s := loadTestState(t, dir)
	if s.LastTestResultKnown || s.TestPasses != 0 || s.LastShipResultKnown || s.ShipCompletions != 0 {
		t.Fatalf("plain text advanced result counters: %#v", s)
	}
}

func TestNoOpTestTokenCannotVerifyRevision(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "fake"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "real edit"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "fake-test", "tool_input": map[string]any{"command": "echo test"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "fake-test", "tool_response": map[string]any{"exit_code": 0}})

	productionCommand := "git " + "push origin main"
	production := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "push", "tool_input": map[string]any{"command": productionCommand}})
	assertMarkedPreToolOutput(t, production)

	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files: %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Tests != 0 || s.VerifiedRevision != 0 {
		t.Fatalf("fake test recorded as verification: %#v", s)
	}
}

func TestReadOnlyInspectionDoesNotEmitGenericPolicy(t *testing.T) {
	dir := t.TempDir()
	var out map[string]any
	for i := 0; i < 8; i++ {
		out = hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "advice", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("read-%d", i),
			"tool_input": map[string]any{"command": fmt.Sprintf("git status --short %d", i)},
		})
	}
	assertNoAdditionalContext(t, out)
	if encoded := string(mustJSON(out)); strings.Contains(encoded, "inspection") || strings.Contains(encoded, "smallest step") {
		t.Fatalf("generic inspection policy leaked into read-only work: %#v", out)
	}
}

func TestGoalModeCarriesAcrossTurnsAndClears(t *testing.T) {
	dir := t.TempDir()
	start := hook(t, dir, map[string]any{
		"session_id": "goal-session", "turn_id": "start", "hook_event_name": "PreToolUse",
		"tool_name": "functions.create_goal", "tool_use_id": "create",
		"tool_input": map[string]any{"objective": "Deliver the requested change"},
	})
	if guidance := hookAdditionalContext(start); !strings.Contains(guidance, "Goal mode started") || !strings.Contains(guidance, "objective") {
		t.Fatalf("goal start lacks coaching: %#v", start)
	}
	hook(t, dir, map[string]any{
		"session_id": "goal-session", "turn_id": "start", "hook_event_name": "PostToolUse",
		"tool_name": "functions.create_goal", "tool_use_id": "create",
		"tool_response": map[string]any{"goal": map[string]any{"status": "active"}},
	})

	continued := hook(t, dir, map[string]any{"session_id": "goal-session", "turn_id": "continued", "hook_event_name": "SessionStart"})
	if guidance := hookAdditionalContext(continued); !strings.Contains(guidance, "Keep the active goal in scope") {
		t.Fatalf("active goal was omitted from session coaching: %#v", continued)
	}

	for i := 1; i <= 40; i++ {
		out := hook(t, dir, map[string]any{
			"session_id": "goal-session", "turn_id": "continued", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("goal-%d", i),
			"tool_input": map[string]any{"command": fmt.Sprintf("echo step-%d", i)},
		})
		if strings.Contains(string(mustJSON(out)), "tool calls") {
			t.Fatalf("generic call-volume policy leaked into goal mode: %#v", out)
		}
	}
	p, err := statePath(event{SessionID: "goal-session", TurnID: "continued"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadState(p, "goal-session", "continued")
	if err != nil || !s.GoalScoped {
		t.Fatalf("continued state = %#v err=%v", s, err)
	}

	hook(t, dir, map[string]any{
		"session_id": "goal-session", "turn_id": "finish", "hook_event_name": "PreToolUse",
		"tool_name": "functions.update_goal", "tool_use_id": "finish",
		"tool_input": map[string]any{"status": "complete"},
	})
	hook(t, dir, map[string]any{
		"session_id": "goal-session", "turn_id": "finish", "hook_event_name": "PostToolUse",
		"tool_name": "functions.update_goal", "tool_use_id": "finish",
		"tool_response": map[string]any{"goal": map[string]any{"status": "complete"}},
	})
	ended := hook(t, dir, map[string]any{"session_id": "goal-session", "turn_id": "after", "hook_event_name": "SessionStart"})
	if strings.Contains(string(mustJSON(ended)), "Goal active") {
		t.Fatalf("completed goal remained active: %#v", ended)
	}
}

func TestGoalModeDoesNotScoreToolVolume(t *testing.T) {
	normal := state{TotalCalls: 100, CallCostUnits: 400, PassiveWaits: 1}
	goal := normal
	goal.GoalScoped = true
	if got := numericScore(normal); got != 78 {
		t.Fatalf("normal score = %d, want 78", got)
	}
	if got := numericScore(goal); got != 93 {
		t.Fatalf("goal score = %d, want 93", got)
	}
	goalReport := reportLine(goal)
	if !strings.Contains(goalReport, "Mode: /goal; tool-call volume is not scored") || !strings.Contains(goalReport, "Activity score: 93/100 (diagnostic; tool-call volume not scored)") {
		t.Fatalf("goal report = %s", goalReport)
	}
	normalReport := reportLine(normal)
	if strings.Contains(normalReport, "/goal") || strings.Contains(normalReport, "volume not scored") || !strings.Contains(normalReport, "Activity score: 78/100 (diagnostic)") {
		t.Fatalf("normal report = %s", normalReport)
	}
	if got := goalTransition(event{ToolName: "functions.update_goal", ToolInput: json.RawMessage(`{"status":"blocked"}`)}); got != "finish" {
		t.Fatalf("blocked goal transition = %q", got)
	}
}

func TestVerifiedStopAdvisesUntilShipItSucceeds(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "e", "tool_input": map[string]any{"command": "patch"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "e", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: VERIFIED") || !strings.Contains(out["systemMessage"].(string), "Run ship-it") {
		t.Fatalf("verified stop was not advisory: %#v", out)
	}
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "Done"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: VERIFIED") || !strings.Contains(out["systemMessage"].(string), "Run ship-it") {
		t.Fatalf("repeated verified stop was not advisory: %#v", out)
	}
	if _, err := loadLifetime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("premature stop recorded lifetime success: %v", err)
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "Done"})
	if out["decision"] == "block" {
		t.Fatalf("successful ship-it did not close delivery: %#v", out)
	}
	life, err := loadLifetime()
	if err != nil {
		t.Fatal(err)
	}
	if life.Runs != 1 || life.VerifiedRuns != 1 || life.RevisionVerifiedRuns != 1 {
		t.Fatalf("unexpected lifetime: %#v", life)
	}
}

func TestUnknownDeliveryStopRemainsAdvisory(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})

	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "Stop"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "returned no result") || strings.Contains(out["systemMessage"].(string), "Inspect") {
		t.Fatalf("unresolved delivery stop was not advisory: %#v", out)
	}
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-delivery", "hook_event_name": "Stop", "stop_hook_active": true})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "returned no result") || strings.Contains(out["systemMessage"].(string), "Inspect") {
		t.Fatalf("repeated unknown delivery stop was not advisory: %#v", out)
	}
	if _, err := loadLifetime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unresolved delivery recorded lifetime success: %v", err)
	}
}

func TestUnverifiedStopAdvisesThenVerifiedStopRemainsAdvisory(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	message, _ := out["systemMessage"].(string)
	if out["decision"] == "block" || !strings.Contains(message, "Recorded outcome: ACTIVITY OBSERVED") || !strings.Contains(message, "Run the relevant check") {
		t.Fatalf("unverified stop was not advisory: %#v", out)
	}
	if _, err := loadLifetime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("continuing stop recorded a premature lifetime result: %v", err)
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: VERIFIED") || !strings.Contains(out["systemMessage"].(string), "Run ship-it") {
		t.Fatalf("verified continuation was not advisory: %#v", out)
	}
	if _, err := loadLifetime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("undelivered verified continuation recorded lifetime success: %v", err)
	}
}

func TestStopWithoutToolActivityDoesNotRecordVerifiedLifetime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	beforeLife, beforeErr := loadLifetime()
	if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
		t.Fatalf("unexpected pre-life state: %v", beforeErr)
	}

	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "no-progress",
		"hook_event_name": "Stop", "last_assistant_message": "Nothing was done",
	})
	message, _ := out["systemMessage"].(string)
	if !strings.Contains(message, "Recorded outcome: NO OBSERVED WORK") || !strings.Contains(message, "Tool calls: 0") || strings.Contains(strings.ToLower(message), "authorization") || strings.Contains(strings.ToLower(message), "trust") {
		t.Fatalf("empty stop message was not plain: %#v", out)
	}

	afterLife, err := loadLifetime()
	if beforeErr == nil {
		if err != nil {
			t.Fatalf("lifetime disappeared after stop: %v", err)
		}
		if afterLife.VerifiedRuns != beforeLife.VerifiedRuns || afterLife.Runs != beforeLife.Runs {
			t.Fatalf("verified lifetime advanced without progress: before=%#v after=%#v", beforeLife, afterLife)
		}
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lifetime was created on empty stop: %#v", afterLife)
	}
}

func TestReadOnlyStatusReportsActivityWithoutClaimingVerification(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "read-only", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "inspect", "tool_input": map[string]any{"command": "git status --short"},
	})
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files: %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.TotalCalls != 1 {
		t.Fatalf("read-only command not counted as activity: %#v", s)
	}
	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "read-only", "hook_event_name": "Stop",
		"last_assistant_message": "Reviewed repo state",
	})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("read-only activity was treated as verification: %#v", out)
	}
}

func TestPassiveWaitReportsActivityAndKeepsPenalty(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "passive", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "wait", "tool_input": map[string]any{"command": "sleep 10"},
	})
	if guidance := hookAdditionalContext(out); !strings.Contains(guidance, "Long work can run in the background") {
		t.Fatalf("passive wait lacks coaching: %#v", out)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files: %v, %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.TotalCalls != 1 || s.PassiveWaits != 1 {
		t.Fatalf("passive wait accounting is wrong: %#v", s)
	}
	out = hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "passive", "hook_event_name": "Stop",
		"last_assistant_message": "Still waiting",
	})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("passive wait was treated as verification: %#v", out)
	}
}

func TestAnyToolActivityIsObservedWithoutSemanticGuessing(t *testing.T) {
	dir := t.TempDir()
	for i, tool := range []string{"Bash", "collaborationspawn_agent", "mcp__database__query", "request_user_input"} {
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "activity", "hook_event_name": "PreToolUse",
			"tool_name": tool, "tool_use_id": fmt.Sprintf("tool-%d", i), "tool_input": map[string]any{"command": "true"},
		})
	}
	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "activity", "hook_event_name": "Stop",
		"last_assistant_message": "Activity occurred",
	})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("tool activity was semantically misclassified: %#v", out)
	}
}

func TestNamespacedEditRequiresVerification(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "namespaced-edit", "hook_event_name": "PreToolUse",
		"tool_name": "functions.apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "namespaced-edit", "hook_event_name": "PostToolUse",
		"tool_name": "functions.apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "namespaced-edit", "hook_event_name": "Stop",
		"last_assistant_message": "Edited without testing",
	})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("unverified namespaced edit was marked success: %#v", out)
	}
}

func TestEditToolRecognitionUsesExactToolBasename(t *testing.T) {
	for _, tool := range []string{"apply_patch", "Edit", "Write", "functions.apply_patch", "mcp__filesystem__write"} {
		if !isEditTool(tool) {
			t.Fatalf("recognized edit tool %q was missed", tool)
		}
	}
	for _, tool := range []string{"overwrite", "copyright", "functions.rewrite_prompt", "spawn_agent"} {
		if isEditTool(tool) {
			t.Fatalf("non-edit tool %q was treated as an edit", tool)
		}
	}
}

func TestStandaloneTestCommandRejectsMaskedOrMutatingChains(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"go test ./...", true},
		{"go test ./... -run 'One|Two'", true},
		{"go test ./... # || true", true},
		{"go test ./... || true", false},
		{"go test ./... && sed -i s/old/new/ app.go", false},
		{"go test ./...; true", false},
		{"go test ./... $(touch changed)", false},
		{"go test ./... `touch changed`", false},
	}
	for _, test := range tests {
		if got := standaloneTestCommand(test.command); got != test.want {
			t.Fatalf("standaloneTestCommand(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestLegacyStateDoesNotInferProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	legacy := state{StateVersion: 2, SessionID: "s", TurnID: "legacy", TotalCalls: 5, CallCostUnits: 20}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadState(path, "s", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateVersion != 4 || recordedOutcome(loaded) != outcomeActivity || finalPassed(loaded) {
		t.Fatalf("legacy state inferred progress: %#v", loaded)
	}
}

func TestStatusMigratesLegacyStateAndOmitsSuccessAlias(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	legacy := state{StateVersion: 3, SessionID: "s", TurnID: "legacy-status", TotalCalls: 1}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legacy.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := printLatestTo(&output, true); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	statusState, ok := report["state"].(map[string]any)
	if !ok || statusState["state_version"] != float64(4) {
		t.Fatalf("legacy status was not normalized: %#v", report)
	}
	if _, exists := report["success"]; exists {
		t.Fatalf("status retained ambiguous success alias: %#v", report)
	}
	if report["outcome"] != outcomeActivity || report["verified"] != false {
		t.Fatalf("legacy status inferred verification: %#v", report)
	}
}

func TestStatusPrefersLatestWorkingSessionOverNewerEmptyTurn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	working := state{StateVersion: 4, SessionID: "working", TurnID: "worked", TotalCalls: 1, UpdatedAt: time.Now().Add(-time.Minute)}
	empty := state{StateVersion: 4, SessionID: "empty", TurnID: "new-question", UpdatedAt: time.Now()}
	for name, candidate := range map[string]state{"working.json": working, "empty.json": empty} {
		b, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := printLatestTo(&output, true); err != nil {
		t.Fatal(err)
	}
	var report struct {
		State state `json:"state"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.State.SessionID != "working" || report.State.TurnID != "worked" {
		t.Fatalf("status selected empty turn: %#v", report.State)
	}
}

func TestRequiredTestingOutweighsEfficiency(t *testing.T) {
	unverified := state{Revision: 1}
	if finalPassed(unverified) || numericScore(unverified) != 25 {
		t.Fatalf("unverified edit reported success: score=%d", numericScore(unverified))
	}
	if !strings.Contains(reportLine(unverified), "Recorded outcome: ACTIVITY OBSERVED") {
		t.Fatalf("unverified edit reported success: %s", reportLine(unverified))
	}
	verified := state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
	if !finalPassed(verified) || numericScore(verified) != 100 {
		t.Fatalf("verified efficient run not successful: score=%d", numericScore(verified))
	}
}

func TestVerifiedSmsbridgeScaleRunRemainsSuccess(t *testing.T) {
	s := state{
		TotalCalls: 80, CallCostUnits: 320, Tests: 2, TestPasses: 2,
		Revision: 14, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 14,
		VerifiedRevision: 14, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 1, MaxInspectionStreak: 10, PassiveWaits: 1,
		TotalTestMillis: 11_721, MaxTestMillis: 11_640,
	}
	if score := numericScore(s); score != 73 || !finalPassed(s) {
		t.Fatalf("verified high-cost run lost success: score=%d", score)
	}
	if report := reportLine(s); !strings.HasPrefix(report, "Recorded outcome: VERIFIED") || !strings.Contains(report, "Activity score: 73/100 (diagnostic)") || strings.Contains(report, "Discipline score") {
		t.Fatalf("verified run report contradicts completion: %s", report)
	}
}

func TestVerifiedDeliveredBetterArgoScaleRunRemainsSuccess(t *testing.T) {
	s := state{
		TotalCalls: 73, CallCostUnits: 292, Tests: 5, TestPasses: 4,
		Revision: 15, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 15,
		VerifiedRevision: 15, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 2, ProductionCompletions: 1, PassiveWaits: 1,
		TotalTestMillis: 3_004, MaxTestMillis: 985, RedundantTestMillis: 2_099,
	}
	if score := numericScore(s); score != 82 || !finalPassed(s) || !strings.HasPrefix(reportLine(s), "Recorded outcome: VERIFIED") {
		t.Fatalf("verified delivered run lost success: score=%d report=%s", score, reportLine(s))
	}
}

func TestUnverifiedAndFailedOutcomesRemainDistinct(t *testing.T) {
	tests := []struct {
		state state
		want  string
	}{
		{state: state{TotalCalls: 1, Revision: 1}, want: outcomeActivity},
		{state: state{TotalCalls: 1, Revision: 1, Tests: 1, TestFailures: 1, LastTestResultKnown: true}, want: outcomeFailed},
	}
	for _, test := range tests {
		if got := recordedOutcome(test.state); got != test.want || finalPassed(test.state) {
			t.Fatalf("outcome = %q, want %q: %#v", got, test.want, test.state)
		}
	}
}

func TestLongRedundantTestChainsLoseScore(t *testing.T) {
	efficient := state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true, TotalTestMillis: 240_000, MaxTestMillis: 180_000}
	if numericScore(efficient) != 100 || !finalPassed(efficient) {
		t.Fatalf("necessary test time penalized too early: %d", numericScore(efficient))
	}
	redundant := efficient
	redundant.Tests = 4
	redundant.TotalTestMillis = 840_000
	redundant.RedundantTestMillis = 600_000
	if numericScore(redundant) >= 70 || !finalPassed(redundant) {
		t.Fatalf("redundant chain signal or success is wrong: %d", numericScore(redundant))
	}
}

func TestDurationComesFromActualToolResponse(t *testing.T) {
	raw := json.RawMessage(`{"result":{"wall_time_seconds":61.25}}`)
	if got := testDurationMillis(raw, time.Time{}); got != 61_250 {
		t.Fatalf("duration = %d", got)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func hookAdditionalContext(output map[string]any) string {
	specific, ok := output["hookSpecificOutput"].(map[string]any)
	if !ok {
		return ""
	}
	value, ok := specific["additionalContext"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func assertNoAdditionalContext(t *testing.T, output map[string]any) {
	t.Helper()
	if text := hookAdditionalContext(output); strings.TrimSpace(text) != "" {
		t.Fatalf("unexpected additionalContext guidance: %q", text)
	}
}

func assertEmptyHookOutput(t *testing.T, output map[string]any) {
	t.Helper()
	if encoded := string(mustJSON(output)); encoded != "{}" {
		t.Fatalf("expected empty hook output, got %s", encoded)
	}
}

func assertMarkedPreToolOutput(t *testing.T, output map[string]any) {
	t.Helper()
	specific, ok := output["hookSpecificOutput"].(map[string]any)
	if !ok || specific["permissionDecision"] != "allow" {
		t.Fatalf("missing marked command response: %#v", output)
	}
	updated, ok := specific["updatedInput"].(map[string]any)
	command, commandOK := updated["command"].(string)
	if !ok || !commandOK || !strings.Contains(command, "__ONE_SHOT_TALLY_RESULT_") {
		t.Fatalf("missing result marker: %#v", output)
	}
}

func TestSparkCallsAreDiscounted(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 8; i++ {
		hook(t, dir, map[string]any{"session_id": "s", "turn_id": "spark", "hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "tool_use_id": fmt.Sprintf("spark-%d", i), "tool_input": map[string]any{"agent_type": "spark_worker", "task_name": fmt.Sprintf("bounded-%d", i)}})
	}
	for i := 0; i < 29; i++ {
		hook(t, dir, map[string]any{"session_id": "s", "turn_id": "spark", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": fmt.Sprintf("regular-%d", i), "tool_input": map[string]any{"command": fmt.Sprintf("true #%d", i)}})
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	b, _ := os.ReadFile(files[0])
	var s state
	_ = json.Unmarshal(b, &s)
	if s.TotalCalls != 37 || s.SparkCalls != 8 || s.CallCostUnits != 124 {
		t.Fatalf("unexpected Spark accounting: %#v", s)
	}
	if got := numericScore(s); got != 99 {
		t.Fatalf("discounted score = %d, want 99", got)
	}
	if report := reportLine(s); !strings.Contains(report, "8 Spark; 31 weighted") {
		t.Fatalf("report omits Spark discount: %q", report)
	}
}

func TestHelpDocumentsGoalResumeWithoutPolicyDump(t *testing.T) {
	var out bytes.Buffer
	printHelp(&out)
	for _, want := range []string{"status [--json]", "goal list [--all]", "goal show ID", "goal resume ID", "credential key-check", "Run status"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "Guidance:") || len(out.String()) > 2000 {
		t.Fatalf("help is too verbose: %d bytes", len(out.String()))
	}
}

func TestVersionCreditsColinKnapp(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)
	for _, want := range []string{"one-shot-tally 1.20.0", "ColinKnapp.com"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("version missing %q: %s", want, out.String())
		}
	}
}

func TestConcurrentHooksPreserveEveryCallAndValidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	const calls = 32
	errCh := make(chan error, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			input, err := json.Marshal(map[string]any{
				"session_id": "concurrent", "turn_id": "same-turn", "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_use_id": fmt.Sprintf("call-%d", i),
				"tool_input": map[string]any{"command": fmt.Sprintf("git status --short %d", i)},
			})
			if err == nil {
				err = runHook(bytes.NewReader(input), io.Discard)
			}
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	s := loadTestState(t, dir)
	if s.TotalCalls != calls || len(s.Pending) != calls {
		t.Fatalf("concurrent state lost calls: total=%d pending=%d", s.TotalCalls, len(s.Pending))
	}
}

func TestHookRecoversAndPreservesCorruptState(t *testing.T) {
	for _, test := range []struct {
		name      string
		contents  func(state) []byte
		wantCalls int
	}{
		{name: "valid prefix with trailing fragment", wantCalls: 5, contents: func(s state) []byte {
			b, _ := json.Marshal(s)
			return append(b, []byte(`":2,"started_at":"stale"}`)...)
		}},
		{name: "unrecoverable object", wantCalls: 1, contents: func(state) []byte { return []byte(`{"broken"`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ONE_SHOT_STATE_DIR", dir)
			e := event{SessionID: "recover", TurnID: test.name}
			path, err := statePath(e)
			if err != nil {
				t.Fatal(err)
			}
			original := emptyState(e.SessionID, e.TurnID)
			original.TotalCalls = 4
			if err := os.WriteFile(path, test.contents(original), 0o600); err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]any{
				"session_id": e.SessionID, "turn_id": e.TurnID, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_use_id": "recovered", "tool_input": map[string]any{"command": "git status --short"},
			})
			if err := runHook(bytes.NewReader(input), io.Discard); err != nil {
				t.Fatal(err)
			}
			s := loadTestState(t, dir)
			if s.TotalCalls != test.wantCalls {
				t.Fatalf("calls=%d, want %d", s.TotalCalls, test.wantCalls)
			}
			backups, err := filepath.Glob(path + ".corrupt-*")
			if err != nil || len(backups) != 1 {
				t.Fatalf("corrupt backup=%v err=%v", backups, err)
			}
		})
	}
}

func TestInstallerPrintsColinKnapp(t *testing.T) {
	installHome := t.TempDir()
	cmd := exec.Command("sh", "./install.sh")
	cmd.Env = append(os.Environ(), "ONE_SHOT_INSTALL_HOME="+installHome)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	for _, want := range []string{"one-shot-tally 1.20.0 | ColinKnapp.com", "one-shot-tally: production install verified"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("install output misses %q: %s", want, out)
		}
	}
	installedBinary := filepath.Join(installHome, ".local", "bin", "one-shot-tally")
	installedSkill := filepath.Join(installHome, ".codex", "skills", "one-shot-tally", "SKILL.md")
	for _, path := range []string{installedBinary, installedSkill} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("installed file %s: %v", path, err)
		}
	}
	wantSkill, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	gotSkill, err := os.ReadFile(installedSkill)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSkill, wantSkill) {
		t.Fatal("installed skill differs from repository SKILL.md")
	}
}

func TestPatchContentsCannotForgeCommandEvents(t *testing.T) {
	dir := t.TempDir()
	edit := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "patch", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"cmd": "go test ./...; git push origin main"}})
	assertNoAdditionalContext(t, edit)
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	b, _ := os.ReadFile(files[0])
	var s state
	_ = json.Unmarshal(b, &s)
	if s.Tests != 0 || s.Revision != 1 {
		t.Fatalf("patch contents treated as shell commands: %#v", s)
	}
}

func TestSuccessfulVerifiedProductionIsRecorded(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "delivered"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "safe edit"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	production := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
	assertMarkedPreToolOutput(t, production)
	if !strings.Contains(hookAdditionalContext(production), "confirm the repository") {
		t.Fatalf("delivery lacks target coaching: %#v", production)
	}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
	s := loadTestState(t, dir)
	if s.ProductionCompletions != 1 || !strings.Contains(reportLine(s), "Delivery: 1 completed") {
		t.Fatalf("production completion was not recorded: %#v report=%s", s, reportLine(s))
	}
}

func TestBackgroundRecordCompletesAndWakesWithoutPolling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("TMUX_PANE", "%7")
	logPath := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundCommand([]string{"record", "compile-docs", "--cleanup", "tmux kill-session -t compile-docs"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "background complete compile-docs --wake") {
		t.Fatalf("record lacks completion instruction: %s", out.String())
	}
	jobs, err := loadBackgroundJobs()
	if err != nil || jobs["compile-docs"].Cleanup != "tmux kill-session -t compile-docs" {
		t.Fatalf("job record = %#v, err=%v", jobs, err)
	}
	out.Reset()
	if err := backgroundCommand([]string{"complete", "compile-docs", "--wake"}, &out); err != nil {
		t.Fatal(err)
	}
	jobs, _ = loadBackgroundJobs()
	if jobs["compile-docs"].CompletedAt.IsZero() || !strings.Contains(out.String(), "cleanup:") {
		t.Fatalf("job not completed with cleanup reminder: %#v %s", jobs, out.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(log), "send-keys -l -t %7 Background job compile-docs completed") || strings.Contains(string(log), "belongs to the active task") || strings.Contains(string(log), "cleanup record") || strings.Contains(string(log), "tmux kill-session") || !strings.Contains(string(log), "send-keys -t %7 Enter") {
		t.Fatalf("origin pane was not woken: %q err=%v", log, err)
	}
	completedAt := jobs["compile-docs"].CompletedAt
	out.Reset()
	if err := backgroundCommand([]string{"complete", "compile-docs", "--wake"}, &out); err != nil {
		t.Fatal(err)
	}
	jobs, _ = loadBackgroundJobs()
	if !jobs["compile-docs"].CompletedAt.Equal(completedAt) || !strings.Contains(out.String(), "already completed; no wake sent") {
		t.Fatalf("duplicate completion was not idempotent: %#v %s", jobs["compile-docs"], out.String())
	}
	log, err = os.ReadFile(logPath)
	if err != nil || strings.Count(string(log), "send-keys -l -t %7") != 1 || strings.Count(string(log), "send-keys -t %7 Enter") != 1 {
		t.Fatalf("duplicate completion sent another wake: %q err=%v", log, err)
	}
}

func TestManualBackgroundCompletionDoesNotInjectPaneInput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("TMUX_PANE", "%7")
	logPath := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundCommand([]string{"record", "manual", "--cleanup", "true"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := backgroundCommand([]string{"complete", "manual"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manual completion injected pane input: err=%v", err)
	}
	if !strings.Contains(out.String(), "Completed background job manual") {
		t.Fatalf("manual completion output = %q", out.String())
	}
}

func TestFailedBackgroundWakeIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("TMUX_PANE", "%7")
	logPath := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundCommand([]string{"record", "failed-wake", "--cleanup", "true"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := backgroundCommand([]string{"complete", "failed-wake", "--wake"}, &out)
	if err == nil || !strings.Contains(err.Error(), "wake command failed") {
		t.Fatalf("wake failure = %v", err)
	}
	if err := backgroundCommand([]string{"complete", "failed-wake", "--wake"}, &out); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(log), "send-keys") != 1 {
		t.Fatalf("failed wake was retried: %q err=%v", log, err)
	}
}

func TestConcurrentBackgroundCompletionSendsOneWake(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("TMUX_PANE", "%7")
	logPath := filepath.Join(dir, "tmux.log")
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	tmuxPath := filepath.Join(dir, "tmux")
	if err := os.WriteFile(tmuxPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_TMUX_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := backgroundCommand([]string{"record", "concurrent", "--cleanup", "true"}, &out); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			errs <- backgroundCommand([]string{"complete", "concurrent", "--wake"}, io.Discard)
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	log, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(log), "send-keys -l -t %7") != 1 || strings.Count(string(log), "send-keys -t %7 Enter") != 1 {
		t.Fatalf("concurrent completion sent duplicate wake: %q err=%v", log, err)
	}
}

func TestBackgroundStewardshipRewardAndPassiveWaitPenalty(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "background"}
	record := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "record", "tool_input": map[string]any{"command": "one-shot-tally background record docs --cleanup 'tmux kill-session -t docs'"}})
	assertEmptyHookOutput(t, record)
	if strings.Contains(string(mustJSON(record)), "passive waiting") {
		t.Fatalf("record misclassified as wait: %#v", record)
	}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "record", "tool_response": map[string]any{"exit_code": 0}})
	wait := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "wait", "tool_use_id": "wait", "tool_input": map[string]any{}})
	if guidance := hookAdditionalContext(wait); !strings.Contains(guidance, "Long work can run in the background") {
		t.Fatalf("wait lacks coaching: %#v", wait)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	b, _ := os.ReadFile(files[0])
	var s state
	_ = json.Unmarshal(b, &s)
	if s.BackgroundRecords != 1 || s.PassiveWaits != 1 || numericScore(s) != 98 {
		t.Fatalf("stewardship accounting = %#v score=%d", s, numericScore(s))
	}
	if report := reportLine(s); !strings.Contains(report, "Background: 1 recorded; 0 completed; 1 passive waits") {
		t.Fatalf("report omits stewardship: %q", report)
	}
}

func TestOpaqueCommandResponsesDoNotAdvanceCounters(t *testing.T) {
	dir := t.TempDir()
	commands := []struct {
		id      string
		command string
	}{
		{id: "background", command: "one-shot-tally background record docs --cleanup true"},
		{id: "todo", command: "one-shot-tally todo add later --context outside"},
	}
	for _, command := range commands {
		pre := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "opaque-command", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": command.id, "tool_input": map[string]any{"command": command.command},
		})
		assertEmptyHookOutput(t, pre)
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "opaque-command", "hook_event_name": "PostToolUse",
			"tool_name": "Bash", "tool_use_id": command.id, "tool_response": map[string]any{},
		})
	}
	s := loadTestState(t, dir)
	if s.BackgroundRecords != 0 || s.TodosParked != 0 {
		t.Fatalf("opaque responses advanced counters: %#v", s)
	}
}

func TestDetachedTmuxWithoutRecordGetsCoaching(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "tmux", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "tmux", "tool_input": map[string]any{"command": "tmux new-session -d -s build 'make all'"}})
	if guidance := hookAdditionalContext(out); !strings.Contains(guidance, "Record this background job") || !strings.Contains(guidance, "cleanup command") {
		t.Fatalf("detached job lacks coaching: %#v", out)
	}
}

func TestPassivePollingIsPenalizedButOrdinaryReadIsNot(t *testing.T) {
	base := state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
	penalized := base
	penalized.PassiveWaits = 2
	if numericScore(penalized) != 86 || numericScore(base) != 100 {
		t.Fatalf("passive penalty=%d baseline=%d", numericScore(penalized), numericScore(base))
	}
	if passiveWait(event{ToolName: "Bash"}, "git status --short") || passiveWait(event{ToolName: "collaborationwait_agent"}, "") || !passiveWait(event{ToolName: "Bash"}, "while true; do tmux has-session -t build; sleep 5; done") {
		t.Fatal("poll classifier over- or under-matched")
	}
}

func TestDeliveryContractChangesAreNotBlocked(t *testing.T) {
	dir := t.TempDir()
	patch := "*** Begin Patch\n*** Delete File: .woodpecker.yml\n*** Update File: README.md\n@@\n-Pushing main triggers .woodpecker.yml, which deploys after convergence.\n+Run deployment commands manually.\n*** End Patch"
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "contract", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "delete", "tool_input": map[string]any{"patch": patch}})
	assertEmptyHookOutput(t, out)
}

func TestDirectDeliveryEntrypointRemovalIsNotBlocked(t *testing.T) {
	for _, command := range []string{"rm scripts/ship.sh", "git rm -- deploy.sh", "rm .woodpecker.yml", "git rm .github/workflows/deploy.yml"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "delete", "tool_input": map[string]any{"command": command}})
			assertEmptyHookOutput(t, out)
		})
	}
}

func TestSessionStartUsesPlainCoaching(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "guidance", "hook_event_name": "SessionStart"})
	guidance := hookAdditionalContext(out)
	for _, want := range []string{"Finish the current request", "Verify changes", "Use ship-it"} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("session coaching missing %q: %#v", want, out)
		}
	}
	if len(guidance) > 160 {
		t.Fatalf("session coaching is too long: %q", guidance)
	}
}

func TestUserPromptSubmitCoachesCorrections(t *testing.T) {
	dir := t.TempDir()
	ordinary := hook(t, dir, map[string]any{"session_id": "ordinary", "turn_id": "prompt", "hook_event_name": "UserPromptSubmit", "prompt": "Please finish the report"})
	assertEmptyHookOutput(t, ordinary)
	corrections := []struct {
		prompt string
		want   string
	}{
		{prompt: "Use repository B, not repository A.", want: "Apply the correction"},
		{prompt: "Actually, switch from main to release.", want: "Another correction came in"},
		{prompt: "No, stop and recheck the repository.", want: "Corrections are repeating"},
	}
	for index, correction := range corrections {
		out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": fmt.Sprintf("prompt-%d", index), "hook_event_name": "UserPromptSubmit", "prompt": correction.prompt})
		if guidance := hookAdditionalContext(out); !strings.Contains(guidance, correction.want) || len(guidance) > 100 {
			t.Fatalf("correction %d coaching = %#v", index+1, out)
		}
	}
}

func TestCorrectionCoachingResetsAfterProgress(t *testing.T) {
	dir := t.TempDir()
	for _, prompt := range []string{"Stop, use the other repository.", "Wrong repository again."} {
		hook(t, dir, map[string]any{"session_id": "s", "turn_id": prompt, "hook_event_name": "UserPromptSubmit", "prompt": prompt})
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "work", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "progress"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "work", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "after", "hook_event_name": "UserPromptSubmit", "prompt": "Stop, use the corrected repository."})
	guidance := hookAdditionalContext(out)
	if !strings.Contains(guidance, "Apply the correction") || strings.Contains(guidance, "Another correction") {
		t.Fatalf("progress did not reset correction coaching: %#v", out)
	}
}

func TestSixthCheckGetsReusePrompt(t *testing.T) {
	dir := t.TempDir()
	var out map[string]any
	for i := 1; i <= 6; i++ {
		out = hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "checks", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("check-%d", i),
			"tool_input": map[string]any{"command": fmt.Sprintf("go test ./... -count=%d", i)},
		})
	}
	if guidance := hookAdditionalContext(out); !strings.Contains(guidance, "sixth test run") || !strings.Contains(guidance, "Reuse passing results") {
		t.Fatalf("sixth check lacks reuse coaching: %#v", out)
	}
}

func TestSparkReviewUsesSessionEvidenceOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	verified := state{Revision: 2, SuccessfulEdits: 2, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 2, VerifiedRevision: 2, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true}
	first, err := claimSparkRoutingReview("unused", verified)
	if err != nil || !strings.Contains(first, "No Spark worker was used") || !strings.Contains(first, "independent, low-risk edit") {
		t.Fatalf("Spark review = %q, err=%v", first, err)
	}
	second, err := claimSparkRoutingReview("unused", verified)
	if err != nil || second != "" {
		t.Fatalf("Spark review repeated: %q, err=%v", second, err)
	}
	if err := recordSessionSparkCall("used"); err != nil {
		t.Fatal(err)
	}
	used, err := claimSparkRoutingReview("used", verified)
	if err != nil || used != "" {
		t.Fatalf("Spark use still received a review: %q, err=%v", used, err)
	}
}

func TestHookCoachingUsesPlainLanguage(t *testing.T) {
	dir := t.TempDir()
	var texts []string
	texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "session", "turn_id": "start", "hook_event_name": "SessionStart"})))
	for index, prompt := range []string{"Use repository B, not repository A.", "Wrong repository again.", "No, stop and recheck the repository."} {
		texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "corrections", "turn_id": fmt.Sprintf("c-%d", index), "hook_event_name": "UserPromptSubmit", "prompt": prompt})))
	}
	texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "external", "turn_id": "delete", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "delete", "tool_input": map[string]any{"command": "curl -X DELETE https://example.test/item"}})))
	texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "wait", "turn_id": "wait", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "wait", "tool_input": map[string]any{"command": "sleep 10"}})))
	texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "tmux", "turn_id": "tmux", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "tmux", "tool_input": map[string]any{"command": "tmux new-session -d -s build true"}})))
	for index := 0; index < 2; index++ {
		texts = append(texts, hookAdditionalContext(hook(t, dir, map[string]any{"session_id": "repeat", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": fmt.Sprintf("repeat-%d", index), "tool_input": map[string]any{"command": "git status --short"}})))
	}
	stopOutput := hook(t, dir, map[string]any{"session_id": "idle", "turn_id": "idle", "hook_event_name": "Stop"})
	texts = append(texts, stopOutput["systemMessage"].(string))

	for _, text := range texts {
		lower := strings.ToLower(text)
		for _, banned := range []string{"authorization", "authorized handoff", "deployment trust", "never a gate", "only the user", "cannot infer", "cannot assess"} {
			if strings.Contains(lower, banned) {
				t.Fatalf("coaching contains %q: %q", banned, text)
			}
		}
		if !strings.Contains(text, "\n") && len(text) > 260 {
			t.Fatalf("coaching is too long: %q", text)
		}
	}
}

func TestRepeatedCallsUsePacedCoachingAndResetAfterProgress(t *testing.T) {
	dir := t.TempDir()
	wants := map[int]string{2: "same input again", 4: "repeated this input four times", 7: "keeps repeating this input"}
	for i := 1; i <= 7; i++ {
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("read-%d", i),
			"tool_input": map[string]any{"command": "git status --short"},
		})
		guidance := strings.ToLower(hookAdditionalContext(out))
		if want, ok := wants[i]; ok {
			if !strings.Contains(guidance, want) {
				t.Fatalf("call %d missing %q: %#v", i, want, out)
			}
		} else if guidance != "" {
			t.Fatalf("call %d ignored coaching cooldown: %#v", i, out)
		}
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "progress"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	first := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "after-1", "tool_input": map[string]any{"command": "git status --short"}})
	second := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "after-2", "tool_input": map[string]any{"command": "git status --short"}})
	assertNoAdditionalContext(t, first)
	if !strings.Contains(hookAdditionalContext(second), "same input again") {
		t.Fatalf("progress did not reset repeated-call cadence: first=%#v second=%#v", first, second)
	}
}

func TestDeliveryInvocationClassification(t *testing.T) {
	tests := []struct {
		command          string
		shipping, deploy bool
	}{
		{"ship-it", true, false},
		{"/usr/local/bin/ship-it --remote origin", true, false},
		{"env TOKEN=x ship-it", true, false},
		{"ship-it start", false, false},
		{"git push origin main", false, false},
		{"git push --dry-run origin main", false, false},
		{"deploy-it --commit abc --branch main", false, true},
		{"deploy-it check", false, false},
		{"ship-it && deploy-it --commit abc --branch main", false, false},
		{"ship-it || true", false, false},
		{"deploy-it; true", false, false},
	}
	for _, test := range tests {
		shipping, deploying := deliveryInvocations(test.command)
		if shipping != test.shipping || deploying != test.deploy {
			t.Fatalf("deliveryInvocations(%q) = %v,%v want %v,%v", test.command, shipping, deploying, test.shipping, test.deploy)
		}
	}
}

func TestProductionInvocationClassification(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{"git push origin main", true},
		{"env TOKEN=x git push origin main", true},
		{"git push --dry-run origin main", false},
		{"git push -n origin main", false},
		{"rg 'git push' README.md", false},
		{"echo git push", false},
		{"kubectl apply -f app.yaml", true},
		{"kubectl apply --dry-run=server -f app.yaml", false},
		{"kubectl diff -f app.yaml", false},
		{"docker stack deploy app", true},
		{"docker service update app", true},
		{"terraform apply plan.tfplan", true},
	}
	for _, test := range tests {
		if got := productionInvocation(test.command, false, false); got != test.want {
			t.Fatalf("productionInvocation(%q) = %v, want %v", test.command, got, test.want)
		}
	}
}

func TestVerifiedStopClosesShipAndDeployLoop(t *testing.T) {
	for _, withContract := range []bool{false, true} {
		t.Run(fmt.Sprintf("contract-%v", withContract), func(t *testing.T) {
			dir := t.TempDir()
			repo := t.TempDir()
			if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, out)
			}
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("baseline\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if withContract {
				if err := os.WriteFile(filepath.Join(repo, ".deploy-it.json"), []byte(`{"version":1,"after_ship":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := exec.Command("git", "-C", repo, "add", ".").CombinedOutput(); err != nil {
				t.Fatalf("git add: %v %s", err, out)
			}
			commit := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline")
			if out, err := commit.CombinedOutput(); err != nil {
				t.Fatalf("git commit: %v %s", err, out)
			}
			common := map[string]any{"session_id": "s", "turn_id": "delivery", "cwd": repo}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
			if err := os.WriteFile(filepath.Join(repo, "change.txt"), []byte("changed\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
			ready := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "Stop"})
			message := ready["systemMessage"].(string)
			if ready["decision"] == "block" {
				t.Fatalf("verified undelivered stop was not advisory: %#v", ready)
			}
			if !strings.Contains(message, "Recorded outcome: VERIFIED") || !strings.Contains(message, "Run ship-it") {
				t.Fatalf("verified stop did not close shipping loop: %#v", ready)
			}
			if strings.Contains(strings.ToLower(message), "authorization") || strings.Contains(strings.ToLower(message), "trust") {
				t.Fatalf("verified stop used legal or policy language: %#v", ready)
			}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
			done := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "Stop"})
			doneMessage := done["systemMessage"].(string)
			if withContract && (!strings.Contains(doneMessage, "Shipping is recorded") || !strings.Contains(doneMessage, "Confirm deployment")) {
				t.Fatalf("successful shipping did not preserve deploy proof boundary: %#v", done)
			}
			if !withContract && !strings.Contains(doneMessage, "No deployment setup is configured") {
				t.Fatalf("successful shipping not recorded: %#v", done)
			}
			if done["decision"] == "block" {
				t.Fatalf("successful ship-it did not close delivery: %#v", done)
			}
		})
	}
}

func TestHookNeverSpawnsDeliveryCommands(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "delivery-spawned")
	for _, name := range []string{"ship-it", "deploy-it"} {
		script := filepath.Join(dir, name)
		body := "#!/bin/sh\nprintf spawned > \"" + marker + "\"\n"
		if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	common := map[string]any{"session_id": "s", "turn_id": "no-spawn"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook spawned a delivery command: %v", err)
	}
}

func TestFailedEditCannotBecomeShipReady(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "failed-edit"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "bad change"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 1}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	message := out["systemMessage"].(string)
	if !strings.Contains(message, "Recorded outcome: FAILED") || strings.Contains(message, "Run ship-it") {
		t.Fatalf("failed edit was treated as ship-ready: %#v", out)
	}
}

func TestOutOfOrderEditResultsFollowNewestRevision(t *testing.T) {
	for _, test := range []struct {
		name          string
		newestExit    int
		olderExit     int
		wantShipReady bool
	}{
		{"newest-fails", 1, 0, false},
		{"newest-passes", 0, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			common := map[string]any{"session_id": test.name, "turn_id": "concurrent-edits"}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "older", "tool_input": map[string]any{"patch": "older"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "newest", "tool_input": map[string]any{"patch": "newest"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "newest", "tool_response": map[string]any{"exit_code": test.newestExit}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "older", "tool_response": map[string]any{"exit_code": test.olderExit}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
			out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
			shipReady := strings.Contains(out["systemMessage"].(string), "Run ship-it")
			if shipReady != test.wantShipReady {
				t.Fatalf("newest revision result was overwritten: ready=%v output=%#v", shipReady, out)
			}
		})
	}
}

func TestFailedShipOrDeployGivesRecoveryCoaching(t *testing.T) {
	for _, command := range []string{"ship-it", "deploy-it --commit abc --branch main"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			common := map[string]any{"session_id": "s", "turn_id": command}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "delivery", "tool_input": map[string]any{"command": command}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "delivery", "tool_response": map[string]any{"exit_code": 1}})
			out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
			message := out["systemMessage"].(string)
			if out["decision"] == "block" || !strings.Contains(message, "failed") || !strings.Contains(message, "before retrying") || strings.Contains(strings.ToLower(message), "authorization") || strings.Contains(strings.ToLower(message), "trust") {
				t.Fatalf("failed delivery coaching was wrong: %#v", out)
			}
			out = hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop", "stop_hook_active": true})
			message = out["systemMessage"].(string)
			if out["decision"] == "block" || !strings.Contains(message, "failed") || !strings.Contains(message, "before retrying") {
				t.Fatalf("repeated failed delivery stop was not advisory: %#v", out)
			}
		})
	}
}

func TestFailedDeliveryWithoutLocalEditKeepsSpecificGuidance(t *testing.T) {
	for _, command := range []string{"ship-it", "deploy-it --commit abc --branch main"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_use_id": "delivery", "tool_input": map[string]any{"command": command},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "delivery", "tool_response": map[string]any{"exit_code": 1},
			})
			out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": command, "hook_event_name": "Stop"})
			message := out["systemMessage"].(string)
			if !strings.Contains(message, "Recorded outcome: FAILED") || !strings.Contains(message, "failed") || !strings.Contains(message, "before retrying") || strings.Contains(strings.ToLower(message), "authorization") {
				t.Fatalf("failed delivery without edit lost recovery coaching: %#v", out)
			}
		})
	}
}

func TestCorrectedDeliveryCanResumeAfterFailure(t *testing.T) {
	for _, command := range []string{"ship-it", "deploy-it --commit abc --branch main"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			common := map[string]any{"session_id": "s", "turn_id": command}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "verify-before", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "verify-before", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "failed-delivery", "tool_input": map[string]any{"command": command}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "failed-delivery", "tool_response": map[string]any{"exit_code": 1}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "verify-after", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "verify-after", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "resumed-delivery", "tool_input": map[string]any{"command": command}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "resumed-delivery", "tool_response": map[string]any{"exit_code": 0}})
			out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
			message := out["systemMessage"].(string)
			if strings.Contains(message, "ship-it failed") || strings.Contains(message, "deploy-it failed") || !strings.Contains(strings.ToLower(message), "recorded") {
				t.Fatalf("corrected delivery did not clear earlier failure: %#v", out)
			}
		})
	}
}

func TestSuccessfulDeployResolvesEarlierShipFailure(t *testing.T) {
	dir := t.TempDir()
	repo, _ := committedTestRepo(t, "package app\n")
	headOutput, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOutput))
	common := map[string]any{"session_id": "s", "turn_id": "ship-then-deploy", "cwd": repo}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 1}})
	blocked := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	if blocked["decision"] == "block" || !strings.Contains(blocked["systemMessage"].(string), "ship-it failed") || !strings.Contains(blocked["systemMessage"].(string), "before retrying") {
		t.Fatalf("failed ship-it was not advisory: %#v", blocked)
	}
	preDeploy := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "deploy", "tool_input": map[string]any{"command": "deploy-it --commit " + head + " --branch main"}})
	assertMarkedPreToolOutput(t, preDeploy)
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "deploy", "tool_response": map[string]any{"exit_code": 0}})
	done := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop", "stop_hook_active": true})
	if done["decision"] == "block" || !strings.Contains(strings.ToLower(done["systemMessage"].(string)), "shipping and deployment are recorded") || strings.Contains(done["systemMessage"].(string), "ship-it failed") || strings.Contains(done["systemMessage"].(string), "deploy-it failed") {
		t.Fatalf("successful deploy-it did not resolve earlier ship failure: %#v", done)
	}
}

func TestLaterVerifiedEditRequiresNewShip(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "second-edit"}
	for _, id := range []string{"first", "second"} {
		hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": id + "-edit", "tool_input": map[string]any{"patch": id}})
		hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": id + "-edit", "tool_response": map[string]any{"exit_code": 0}})
		hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": id + "-test", "tool_input": map[string]any{"command": "go test ./..."}})
		hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": id + "-test", "tool_response": map[string]any{"exit_code": 0}})
		if id == "first" {
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
		}
	}
	out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: VERIFIED") || !strings.Contains(out["systemMessage"].(string), "Run ship-it") {
		t.Fatalf("later verified edit inherited older delivery: %#v", out)
	}
}

func TestFailedShipCorrectiveEditStillAdvisesBeforeReverification(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "delivery-recovery"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 1}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "fix", "tool_input": map[string]any{"patch": "fix"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "fix", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "ship-it failed") || !strings.Contains(out["systemMessage"].(string), "before retrying") {
		t.Fatalf("corrective edit lost unresolved delivery guidance: %#v", out)
	}
}

func TestFailedProductionActionPersistsAfterUnrelatedSuccess(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "push-failure", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "push", "tool_input": map[string]any{"command": "git push origin main"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "push-failure", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "push", "tool_response": map[string]any{"exit_code": 1},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "push-failure", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "status", "tool_input": map[string]any{"command": "git status --short"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "push-failure", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "status", "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "push-failure", "hook_event_name": "Stop"})
	message := out["systemMessage"].(string)
	if !strings.Contains(message, "Recorded outcome: FAILED") || !strings.Contains(message, "Delivery failed") || !strings.Contains(message, "before retrying") {
		t.Fatalf("unresolved production failure was overwritten: %#v", out)
	}
}

func TestNewPendingDeliveryInvalidatesOlderSuccess(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "pending-delivery"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "first-ship", "tool_input": map[string]any{"command": "ship-it"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "first-ship", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "pending-ship", "tool_input": map[string]any{"command": "ship-it"}})
	out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	message := out["systemMessage"].(string)
	if !strings.Contains(message, "Recorded outcome: ACTIVITY OBSERVED") || !strings.Contains(message, "returned no result") || strings.Contains(message, "Recorded outcome: VERIFIED") || strings.Contains(message, "Shipping is recorded") {
		t.Fatalf("pending delivery inherited an older success: %#v", out)
	}
}

func TestConcreteProgressResetsPassiveWaitTone(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 6; i++ {
		hook(t, dir, map[string]any{"session_id": "s", "turn_id": "waits", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": fmt.Sprintf("wait-%d", i), "tool_input": map[string]any{"command": fmt.Sprintf("sleep %d", i)}})
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "waits", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "progress"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "waits", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "waits", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "wait-after", "tool_input": map[string]any{"command": "sleep 99"}})
	if guidance := hookAdditionalContext(out); !strings.Contains(guidance, "Long work can run in the background") || strings.Contains(guidance, "still repeating") {
		t.Fatalf("progress did not reset passive-wait coaching: %#v", out)
	}
}

func TestExternalMutationGetsTargetCheckWithoutBlocking(t *testing.T) {
	for i, command := range []string{"ship-it", "deploy-it production", "curl -X DELETE https://vault.example/items/1"} {
		dir := t.TempDir()
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": fmt.Sprintf("target-%d", i), "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": "action", "tool_input": map[string]any{"command": command},
		})
		guidance := hookAdditionalContext(out)
		if !strings.Contains(guidance, "confirm the repository, destination, revision, and expected result") {
			t.Fatalf("missing target coaching for %q: %#v", command, out)
		}
		if strings.Contains(string(mustJSON(out)), `"decision":"block"`) || strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
			t.Fatalf("target coaching blocked %q: %#v", command, out)
		}
	}
}

func TestDuplicateSubagentAndFailedCheckEditsGetAdvisoryCoaching(t *testing.T) {
	dir := t.TempDir()
	input := map[string]any{"agent_type": "explorer", "task_name": "scan", "message": "scan repo"}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "workers", "hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "tool_use_id": "one", "tool_input": input})
	duplicate := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "workers", "hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "tool_use_id": "two", "tool_input": input})
	if guidance := hookAdditionalContext(duplicate); !strings.Contains(guidance, "identical worker") || !strings.Contains(guidance, "different task") {
		t.Fatalf("duplicate worker coaching = %#v", duplicate)
	}

	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 1}})
	edit := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "fix"}})
	if guidance := hookAdditionalContext(edit); !strings.Contains(guidance, "failed check") || !strings.Contains(guidance, "Rerun the affected check") {
		t.Fatalf("failed-check edit coaching = %#v", edit)
	}
}

func TestActiveGoalIsNotReportedComplete(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "start", "hook_event_name": "PreToolUse", "tool_name": "functions.create_goal", "tool_use_id": "create", "tool_input": map[string]any{"objective": "finish"}})
	hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "start", "hook_event_name": "PostToolUse", "tool_name": "functions.create_goal", "tool_use_id": "create", "tool_response": map[string]any{"goal": map[string]any{"status": "active"}}})
	out := hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "later", "hook_event_name": "Stop"})
	message, _ := out["systemMessage"].(string)
	if !strings.Contains(message, "Goal state: ACTIVE") || strings.Contains(message, "Recorded outcome: VERIFIED") {
		t.Fatalf("active goal result = %#v", out)
	}
}

func TestGoalListAndResumeUseCodexHistoryReadOnly(t *testing.T) {
	db := testGoalsDB(t)
	t.Setenv("ONE_SHOT_GOALS_DB", db)

	var out bytes.Buffer
	if err := goalCommand([]string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "paused") || strings.Contains(out.String(), "completed objective") {
		t.Fatalf("resumable list = %q", out.String())
	}

	out.Reset()
	if err := goalCommand([]string{"list", "--all"}, &out); err != nil || !strings.Contains(out.String(), "completed objective") {
		t.Fatalf("all list = %q err=%v", out.String(), err)
	}

	out.Reset()
	if err := goalCommand([]string{"resume", "aaaa"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"paused objective", "Call get_goal", "call create_goal with this objective", "only reads saved goal data"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("resume missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "token_budget") {
		t.Fatalf("unbudgeted resume gained a budget: %s", out.String())
	}

	out.Reset()
	if err := goalCommand([]string{"resume", "dddd"}, &out); err != nil || !strings.Contains(out.String(), "token_budget 500") {
		t.Fatalf("budgeted resume = %q err=%v", out.String(), err)
	}

	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	t.Setenv("ONE_SHOT_GOALS_DB", missing)
	if err := goalCommand([]string{"list"}, &out); err == nil {
		t.Fatal("missing goal database was accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only goal lookup created its database: %v", err)
	}
}

func TestCodexGoalsPathUsesActiveAccountAndExplicitOverride(t *testing.T) {
	root := t.TempDir()
	codexHome := filepath.Join(root, "account")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	accountDB := filepath.Join(codexHome, "goals_1.sqlite")
	if err := os.WriteFile(accountDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("ONE_SHOT_GOALS_DB", "")
	if got, err := codexGoalsPath(); err != nil || got != accountDB {
		t.Fatalf("account goals path = %q, %v; want %q", got, err, accountDB)
	}

	override := filepath.Join(root, "override.sqlite")
	if err := os.WriteFile(override, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONE_SHOT_GOALS_DB", override)
	if got, err := codexGoalsPath(); err != nil || got != override {
		t.Fatalf("override goals path = %q, %v; want %q", got, err, override)
	}

	fallbackHome := filepath.Join(root, "home")
	fallbackDB := filepath.Join(fallbackHome, ".codex", "goals_1.sqlite")
	if err := os.MkdirAll(filepath.Dir(fallbackDB), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fallbackDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ONE_SHOT_GOALS_DB", "")
	t.Setenv("HOME", fallbackHome)
	if got, err := codexGoalsPath(); err != nil || got != fallbackDB {
		t.Fatalf("fallback goals path = %q, %v; want %q", got, err, fallbackDB)
	}
}

func TestSessionStartReconcilesGoalFromActiveAccount(t *testing.T) {
	dir := t.TempDir()
	codexHome := t.TempDir()
	db := testGoalsDB(t)
	if err := os.Rename(db, filepath.Join(codexHome, "goals_1.sqlite")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("ONE_SHOT_GOALS_DB", "")
	t.Setenv("CODEX_HOME", codexHome)

	if err := setSessionGoalActive("thread-complete", true); err != nil {
		t.Fatal(err)
	}
	completed := hook(t, dir, map[string]any{"session_id": "thread-complete", "turn_id": "after", "hook_event_name": "SessionStart"})
	if strings.Contains(string(mustJSON(completed)), "Goal active") {
		t.Fatalf("completed account goal left stale marker: %#v", completed)
	}
	active := hook(t, dir, map[string]any{"session_id": "thread-active", "turn_id": "resume", "hook_event_name": "SessionStart"})
	if !strings.Contains(hookAdditionalContext(active), "Keep the active goal in scope") {
		t.Fatalf("active account goal lacks session coaching: %#v", active)
	}
	if goalActive, err := sessionGoalActive("thread-active"); err != nil || !goalActive {
		t.Fatalf("active account goal was not recognized: active=%v err=%v", goalActive, err)
	}
}

func TestSessionStartReconcilesStaleGoalMarkerFromCodex(t *testing.T) {
	dir := t.TempDir()
	db := testGoalsDB(t)
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	t.Setenv("ONE_SHOT_GOALS_DB", db)
	if err := setSessionGoalActive("thread-complete", true); err != nil {
		t.Fatal(err)
	}
	out := hook(t, dir, map[string]any{"session_id": "thread-complete", "turn_id": "after", "hook_event_name": "SessionStart"})
	if strings.Contains(string(mustJSON(out)), "Goal active") {
		t.Fatalf("completed native goal left stale marker: %#v", out)
	}
	active, err := sessionGoalActive("thread-complete")
	if err != nil || active {
		t.Fatalf("stale marker remains: active=%v err=%v", active, err)
	}

	out = hook(t, dir, map[string]any{"session_id": "thread-active", "turn_id": "resume", "hook_event_name": "SessionStart"})
	if !strings.Contains(hookAdditionalContext(out), "Keep the active goal in scope") {
		t.Fatalf("active native goal lacks session coaching: %#v", out)
	}
	if goalActive, err := sessionGoalActive("thread-active"); err != nil || !goalActive {
		t.Fatalf("active native goal was not recognized: active=%v err=%v", goalActive, err)
	}
}

func testGoalsDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "goals.sqlite")
	schema := `CREATE TABLE thread_goals (
		thread_id TEXT PRIMARY KEY NOT NULL,
		goal_id TEXT NOT NULL,
		objective TEXT NOT NULL,
		status TEXT NOT NULL,
		token_budget INTEGER,
		tokens_used INTEGER NOT NULL DEFAULT 0,
		time_used_seconds INTEGER NOT NULL DEFAULT 0,
		created_at_ms INTEGER NOT NULL,
		updated_at_ms INTEGER NOT NULL
	);
	INSERT INTO thread_goals VALUES ('thread-paused','aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','paused objective','paused',NULL,12,0,1000,2000);
	INSERT INTO thread_goals VALUES ('thread-active','cccccccc-cccc-cccc-cccc-cccccccccccc','active objective','active',NULL,23,0,2000,3000);
	INSERT INTO thread_goals VALUES ('thread-complete','bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb','completed objective','complete',NULL,34,0,3000,4000);
	INSERT INTO thread_goals VALUES ('thread-budget','dddddddd-dddd-dddd-dddd-dddddddddddd','budgeted objective','budget_limited',500,500,0,4000,5000);`
	if output, err := exec.Command("sqlite3", path, schema).CombinedOutput(); err != nil {
		t.Fatalf("create goals fixture: %v: %s", err, output)
	}
	return path
}

func TestTodoLifecycleIsDurableAndDeduplicated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	var out bytes.Buffer
	if err := todoCommand([]string{"add", "Investigate alternate transport", "--context", "Useful, but outside the current delivery goal"}, &out); err != nil {
		t.Fatal(err)
	}
	items, err := loadTodos()
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	var id string
	for id = range items {
	}
	if err := todoCommand([]string{"add", "Investigate alternate transport", "--context", "duplicate"}, &out); err == nil {
		t.Fatal("duplicate TODO was accepted")
	}
	out.Reset()
	if err := todoCommand([]string{"list"}, &out); err != nil || !strings.Contains(out.String(), id+"\topen") {
		t.Fatalf("open list=%q err=%v", out.String(), err)
	}
	out.Reset()
	if err := todoCommand([]string{"done", id}, &out); err != nil {
		t.Fatal(err)
	}
	if err := todoCommand([]string{"done", id}, &out); err == nil {
		t.Fatal("duplicate completion was accepted")
	}
	out.Reset()
	if err := todoCommand([]string{"list"}, &out); err != nil || out.Len() != 0 {
		t.Fatalf("completed TODO remained open: %q err=%v", out.String(), err)
	}
	if err := todoCommand([]string{"list", "--all"}, &out); err != nil || !strings.Contains(out.String(), id+"\tdone") {
		t.Fatalf("all list=%q err=%v", out.String(), err)
	}
}

func TestTodoParkingRewardRequiresVerifiedCurrentOutcome(t *testing.T) {
	verified := state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true, TodosParked: 2}
	if got := numericScore(verified); got != 96 {
		t.Fatalf("verified parking score=%d, want 96", got)
	}
	capped := verified
	capped.TodosParked = 20
	if got := numericScore(capped); got != 98 {
		t.Fatalf("parking reward cap score=%d, want 98", got)
	}
	unverified := state{Revision: 1, TodosParked: 20}
	if got := numericScore(unverified); got != 25 {
		t.Fatalf("unverified work received TODO reward: %d", got)
	}
}

func TestHookCountsSuccessfulTodoTransitions(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "todo"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "add", "tool_input": map[string]any{"command": "one-shot-tally todo add 'later' --context 'outside goal'"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "add", "tool_response": map[string]any{"exit_code": 0}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "done", "tool_input": map[string]any{"command": "one-shot-tally todo done abc123"}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "done", "tool_response": map[string]any{"exit_code": 0}})
	s := loadTestState(t, dir)
	if s.TodosParked != 1 || s.TodosCompleted != 1 || !strings.Contains(reportLine(s), "TODOs: 1 saved; 1 completed") {
		t.Fatalf("TODO hook state=%#v report=%s", s, reportLine(s))
	}
}

func loadTestState(t *testing.T, dir string) state {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("state files: %v err=%v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
