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

func TestMechanicalTallyAndCoachingScore(t *testing.T) {
	dir := t.TempDir()
	base := map[string]any{"session_id": "s", "turn_id": "t", "hook_event_name": "PreToolUse", "tool_name": "Bash"}
	for i := 1; i <= 3; i++ {
		base["tool_use_id"] = string(rune('a' + i))
		base["tool_input"] = map[string]any{"command": "git status --short"}
		out := hook(t, dir, base)
		if i == 2 && !strings.Contains(string(mustJSON(out)), "gentle note") {
			t.Fatalf("missing first repetition steer: %#v", out)
		}
		if i == 3 && strings.Contains(string(mustJSON(out)), "nudge") {
			t.Fatalf("repetition steer ignored its cooldown: %#v", out)
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

func TestDeliveryAndGitCommandsAreNeverDenied(t *testing.T) {
	dir := t.TempDir()
	commands := []string{
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
		if strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
			t.Fatalf("command %q was denied: %#v", command, out)
		}
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
	if strings.Contains(string(mustJSON(production)), `"permissionDecision":"deny"`) {
		t.Fatalf("delivery command was denied: %#v", production)
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
	if text := string(mustJSON(start)); !strings.Contains(text, "Goal mode started") || !strings.Contains(text, "High tool-call volume is expected") || !strings.Contains(text, "spark_worker") || !strings.Contains(text, "security judgment") || !strings.Contains(text, "overlapping ownership") || strings.Contains(text, "non-code agent messages") || strings.Contains(text, "Microsoft-style") {
		t.Fatalf("goal start guidance = %#v", start)
	}
	hook(t, dir, map[string]any{
		"session_id": "goal-session", "turn_id": "start", "hook_event_name": "PostToolUse",
		"tool_name": "functions.create_goal", "tool_use_id": "create",
		"tool_response": map[string]any{"goal": map[string]any{"status": "active"}},
	})

	continued := hook(t, dir, map[string]any{"session_id": "goal-session", "turn_id": "continued", "hook_event_name": "SessionStart"})
	if text := string(mustJSON(continued)); !strings.Contains(text, "Goal active") || !strings.Contains(text, "close it only after verification") {
		t.Fatalf("goal continuation guidance = %#v", continued)
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
	if !strings.Contains(goalReport, "Run mode: /goal (high tool-call volume expected)") || !strings.Contains(goalReport, "Coaching signals: 93/100 (advisory; tool-call volume not scored)") {
		t.Fatalf("goal report = %s", goalReport)
	}
	normalReport := reportLine(normal)
	if strings.Contains(normalReport, "/goal") || strings.Contains(normalReport, "volume not scored") || !strings.Contains(normalReport, "Coaching signals: 78/100 (advisory)") {
		t.Fatalf("normal report = %s", normalReport)
	}
	if got := goalTransition(event{ToolName: "functions.update_goal", ToolInput: json.RawMessage(`{"status":"blocked"}`)}); got != "finish" {
		t.Fatalf("blocked goal transition = %q", got)
	}
}

func TestVerifiedStopReportsWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "e", "tool_input": map[string]any{"command": "patch"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Goal result: SUCCESS") {
		t.Fatalf("unexpected stop output: %#v", out)
	}
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "Done"})
	if out["decision"] == "block" || strings.Contains(out["systemMessage"].(string), "Closing loop:") {
		t.Fatalf("stop looped: %#v", out)
	}
	life, err := loadLifetime()
	if err != nil {
		t.Fatal(err)
	}
	if life.Runs != 1 || life.VerifiedRuns != 1 {
		t.Fatalf("unexpected lifetime: %#v", life)
	}
}

func TestUnverifiedStopAdvisesWithoutBlockingOrLifetimeRecord(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	message, _ := out["systemMessage"].(string)
	if out["decision"] == "block" || !strings.Contains(message, "NOT VERIFIED") || !strings.Contains(message, "smallest goal-directed verification step") {
		t.Fatalf("unverified stop was not advisory: %#v", out)
	}
	if _, err := loadLifetime(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("continuing stop recorded a premature lifetime result: %v", err)
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "continue", "hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "Done"})
	if out["decision"] == "block" {
		t.Fatalf("continued stop looped: %#v", out)
	}
	life, err := loadLifetime()
	if err != nil || life.Runs != 1 || life.VerifiedRuns != 1 {
		t.Fatalf("continued success was not recorded: %#v err=%v", life, err)
	}
}

func TestRequiredTestingOutweighsEfficiency(t *testing.T) {
	unverified := state{Revision: 1}
	if finalPassed(unverified) || numericScore(unverified) != 25 {
		t.Fatalf("unverified edit reported success: score=%d", numericScore(unverified))
	}
	if !strings.Contains(reportLine(unverified), "Goal result: NOT VERIFIED") {
		t.Fatalf("unverified edit reported success: %s", reportLine(unverified))
	}
	verified := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
	if !finalPassed(verified) || numericScore(verified) != 100 {
		t.Fatalf("verified efficient run not successful: score=%d", numericScore(verified))
	}
}

func TestVerifiedSmsbridgeScaleRunRemainsSuccess(t *testing.T) {
	s := state{
		TotalCalls: 80, CallCostUnits: 320, Tests: 2, TestPasses: 2,
		Revision: 14, VerifiedRevision: 14, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 1, MaxInspectionStreak: 10, PassiveWaits: 1,
		TotalTestMillis: 11_721, MaxTestMillis: 11_640,
	}
	if score := numericScore(s); score != 73 || !finalPassed(s) {
		t.Fatalf("verified high-cost run lost success: score=%d", score)
	}
	if report := reportLine(s); !strings.HasPrefix(report, "Goal result: SUCCESS") || !strings.Contains(report, "Coaching signals: 73/100 (advisory)") || strings.Contains(report, "Discipline score") {
		t.Fatalf("verified run report contradicts completion: %s", report)
	}
}

func TestVerifiedDeliveredBetterArgoScaleRunRemainsSuccess(t *testing.T) {
	s := state{
		TotalCalls: 73, CallCostUnits: 292, Tests: 5, TestPasses: 4,
		Revision: 15, VerifiedRevision: 15, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 2, ProductionCompletions: 1, PassiveWaits: 1,
		TotalTestMillis: 3_004, MaxTestMillis: 985, RedundantTestMillis: 2_099,
	}
	if score := numericScore(s); score != 82 || !finalPassed(s) || !strings.HasPrefix(reportLine(s), "Goal result: SUCCESS") {
		t.Fatalf("verified delivered run lost success: score=%d report=%s", score, reportLine(s))
	}
}

func TestUnverifiedOutcomeDoesNotReportSuccess(t *testing.T) {
	for _, s := range []state{
		{Revision: 1},
		{Revision: 1, Tests: 1, TestFailures: 1, LastTestResultKnown: true},
	} {
		if finalPassed(s) || !strings.Contains(reportLine(s), "Goal result: NOT VERIFIED") {
			t.Fatalf("unverified state reported success: %#v report=%s", s, reportLine(s))
		}
	}
}

func TestLongRedundantTestChainsLoseScore(t *testing.T) {
	efficient := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, TotalTestMillis: 240_000, MaxTestMillis: 180_000}
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
	for _, want := range []string{"status [--json]", "goal list [--all]", "goal show ID", "goal resume ID", "score as advisory"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, out.String())
		}
	}
	if strings.Count(out.String(), "Guidance:") != 1 || len(out.String()) > 1800 {
		t.Fatalf("help policy is too verbose: %d bytes", len(out.String()))
	}
}

func TestVersionCreditsColinKnapp(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)
	for _, want := range []string{"one-shot-tally 1.11.1", "ColinKnapp.com"} {
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
	if !strings.Contains(string(out), "ColinKnapp.com") {
		t.Fatalf("install output misses domain: %s", out)
	}
	for _, path := range []string{
		filepath.Join(installHome, ".local", "bin", "one-shot-tally"),
		filepath.Join(installHome, ".codex", "skills", "one-shot-tally", "SKILL.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("installed file %s: %v", path, err)
		}
	}
}

func TestPatchContentsCannotForgeCommandEvents(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "patch", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"cmd": "go test ./...; git push origin main"}})
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
	if strings.Contains(string(mustJSON(production)), `"permissionDecision":"deny"`) {
		t.Fatalf("verified production command was denied: %#v", production)
	}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
	s := loadTestState(t, dir)
	if s.ProductionCompletions != 1 || !strings.Contains(reportLine(s), "Delivery actions: 1 completed") {
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
	if !strings.Contains(out.String(), "background complete compile-docs") {
		t.Fatalf("record lacks completion instruction: %s", out.String())
	}
	jobs, err := loadBackgroundJobs()
	if err != nil || jobs["compile-docs"].Cleanup != "tmux kill-session -t compile-docs" {
		t.Fatalf("job record = %#v, err=%v", jobs, err)
	}
	out.Reset()
	if err := backgroundCommand([]string{"complete", "compile-docs"}, &out); err != nil {
		t.Fatal(err)
	}
	jobs, _ = loadBackgroundJobs()
	if jobs["compile-docs"].CompletedAt.IsZero() || !strings.Contains(out.String(), "cleanup:") {
		t.Fatalf("job not completed with cleanup reminder: %#v %s", jobs, out.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(log), "send-keys -l -t %7 Background job compile-docs completed") || !strings.Contains(string(log), "send-keys -t %7 Enter") {
		t.Fatalf("origin pane was not woken: %q err=%v", log, err)
	}
}

func TestBackgroundStewardshipRewardAndPassiveWaitPenalty(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "background"}
	record := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "record", "tool_input": map[string]any{"command": "one-shot-tally background record docs --cleanup 'tmux kill-session -t docs'"}})
	if strings.Contains(string(mustJSON(record)), "passive waiting") {
		t.Fatalf("record misclassified as wait: %#v", record)
	}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "record", "tool_response": map[string]any{"exit_code": 0}})
	wait := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "wait", "tool_use_id": "wait", "tool_input": map[string]any{}})
	if guidance := string(mustJSON(wait)); !strings.Contains(guidance, "passive waiting adds no evidence") || !strings.Contains(guidance, "record its cleanup and wake-up target") {
		t.Fatalf("wait lacks corrective guidance: %#v", wait)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	b, _ := os.ReadFile(files[0])
	var s state
	_ = json.Unmarshal(b, &s)
	if s.BackgroundRecords != 1 || s.PassiveWaits != 1 || numericScore(s) != 98 {
		t.Fatalf("stewardship accounting = %#v score=%d", s, numericScore(s))
	}
	if report := reportLine(s); !strings.Contains(report, "1 recorded, 0 completed; passive waits: 1") {
		t.Fatalf("report omits stewardship: %q", report)
	}
}

func TestDetachedTmuxWithoutRecordGetsGuidance(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "tmux", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "tmux", "tool_input": map[string]any{"command": "tmux new-session -d -s build 'make all'"}})
	if guidance := string(mustJSON(out)); !strings.Contains(guidance, "record this detached job") || !strings.Contains(guidance, "without polling") {
		t.Fatalf("missing detached-job guidance: %#v", out)
	}
}

func TestPassivePollingIsPenalizedButOrdinaryReadIsNot(t *testing.T) {
	base := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
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
	if strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
		t.Fatalf("delivery change denied: %#v", out)
	}
}

func TestDirectDeliveryEntrypointRemovalIsNotBlocked(t *testing.T) {
	for _, command := range []string{"rm scripts/ship.sh", "git rm -- deploy.sh", "rm .woodpecker.yml", "git rm .github/workflows/deploy.yml"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "delete", "tool_input": map[string]any{"command": command}})
			if strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
				t.Fatalf("direct removal denied: %#v", out)
			}
		})
	}
}

func TestSessionGuidanceIsConcise(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "guidance", "hook_event_name": "SessionStart"})
	text := string(mustJSON(out))
	for _, want := range []string{"Finish the latest requested outcome", "verify edits", "score is advisory", "current repository", "external changes", "explorers for evidence", "workers or implementors for scoped changes", "reviewers for checks", "actively look for an exact, low-risk, independent edit", "spark_worker", "When one exists", "otherwise continue in the main thread", "exact files, expected behavior, and validation", "security judgment", "ship/deploy", "sequential work", "overlapping ownership", "non-code agent messages terse", "preserve exact technical terms"} {
		if !strings.Contains(text, want) {
			t.Fatalf("guidance missing %q: %#v", want, out)
		}
	}
	if len(text) > 1000 {
		t.Fatalf("session guidance is too verbose: %d bytes", len(text))
	}
}

func TestSparkRoutingReviewUsesSessionEvidenceAndAppearsOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ONE_SHOT_STATE_DIR", dir)
	base := state{Revision: 3, SuccessfulEdits: 2, VerifiedRevision: 3, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true}
	first, err := claimSparkRoutingReview("unused", base)
	if err != nil || !strings.Contains(first, "none used this run") || !strings.Contains(first, "spark_worker") || !strings.Contains(first, "do not invent one") {
		t.Fatalf("missing Spark routing review: %q err=%v", first, err)
	}
	second, err := claimSparkRoutingReview("unused", base)
	if err != nil || second != "" {
		t.Fatalf("Spark routing review repeated: %q err=%v", second, err)
	}
	if err := recordSessionSparkCall("used-earlier-turn"); err != nil {
		t.Fatal(err)
	}
	used, err := claimSparkRoutingReview("used-earlier-turn", base)
	if err != nil || used != "" {
		t.Fatalf("session Spark use still received a reminder: %q err=%v", used, err)
	}
	retry := base
	retry.Revision = 2
	retry.SuccessfulEdits = 1
	retry.VerifiedRevision = 2
	if review, err := claimSparkRoutingReview("failed-then-passed", retry); err != nil || review != "" {
		t.Fatalf("one successful edit received a Spark reminder: %q err=%v", review, err)
	}
}

func TestNewestPromptAmendsCompatibleScope(t *testing.T) {
	dir := t.TempDir()
	ordinary := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "prompt", "hook_event_name": "UserPromptSubmit", "prompt": "Please finish the report"})
	if text := string(mustJSON(ordinary)); text != "{}" {
		t.Fatalf("ordinary prompt received generic policy: %#v", ordinary)
	}
	corrected := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "prompt", "hook_event_name": "UserPromptSubmit", "prompt": "Stop, this is meant to deploy ../wmi instead"})
	text := string(mustJSON(corrected))
	if !strings.Contains(text, "Thanks for the correction") || !strings.Contains(text, "conflicting part") || !strings.Contains(text, "confirm the target") || strings.Contains(text, "stop and realign") {
		t.Fatalf("correction guidance = %#v", corrected)
	}
	second := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "second", "hook_event_name": "UserPromptSubmit", "prompt": "Wrong repository; use the named target"})
	if text := string(mustJSON(second)); !strings.Contains(text, "another correction") || !strings.Contains(text, "pause briefly") {
		t.Fatalf("second correction did not increase gently: %#v", second)
	}
	third := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "third", "hook_event_name": "UserPromptSubmit", "prompt": "No, stop and recheck it"})
	if text := string(mustJSON(third)); !strings.Contains(text, "stop and realign") || !strings.Contains(text, "Corrections are repeating") {
		t.Fatalf("repeated corrections did not become clear: %#v", third)
	}
}

func TestCorrectionToneResetsAfterProgress(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "one", "hook_event_name": "UserPromptSubmit", "prompt": "Stop, use the other target"})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "two", "hook_event_name": "UserPromptSubmit", "prompt": "Wrong target again"})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "work", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "progress"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "work", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "three", "hook_event_name": "UserPromptSubmit", "prompt": "Stop, one more correction"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Thanks for the correction") || strings.Contains(text, "another correction") {
		t.Fatalf("progress did not reset correction tone: %#v", out)
	}
}

func TestRepeatedCallSteersUseWideningIntervalsAndReset(t *testing.T) {
	dir := t.TempDir()
	wants := map[int]string{2: "gentle note", 4: "friendly nudge", 7: "clear steer"}
	for i := 1; i <= 7; i++ {
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("read-%d", i),
			"tool_input": map[string]any{"command": "git status --short"},
		})
		text := strings.ToLower(string(mustJSON(out)))
		if want, ok := wants[i]; ok {
			if !strings.Contains(text, want) {
				t.Fatalf("call %d missing %q: %#v", i, want, out)
			}
		} else if strings.Contains(text, "gentle note") || strings.Contains(text, "friendly nudge") || strings.Contains(text, "clear steer") {
			t.Fatalf("call %d ignored widening interval: %#v", i, out)
		}
	}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "progress"}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
	first := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "after-1", "tool_input": map[string]any{"command": "git status --short"}})
	second := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "repeat", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "after-2", "tool_input": map[string]any{"command": "git status --short"}})
	if string(mustJSON(first)) != "{}" || !strings.Contains(strings.ToLower(string(mustJSON(second))), "gentle note") {
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

func TestVerifiedStopClosesShipAndDeployLoop(t *testing.T) {
	for _, withContract := range []bool{false, true} {
		t.Run(fmt.Sprintf("contract-%v", withContract), func(t *testing.T) {
			dir := t.TempDir()
			repo := t.TempDir()
			if withContract {
				if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v %s", err, out)
				}
				if err := os.WriteFile(filepath.Join(repo, ".deploy-it.json"), []byte(`{"version":1,"after_ship":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command("git", "-C", repo, "add", ".deploy-it.json").CombinedOutput(); err != nil {
					t.Fatalf("git add: %v %s", err, out)
				}
			}
			common := map[string]any{"session_id": "s", "turn_id": "delivery", "cwd": repo}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
			ready := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "Stop"})
			message := ready["systemMessage"].(string)
			if !strings.Contains(message, "ship-ready") || !strings.Contains(message, "Run ship-it") {
				t.Fatalf("verified stop did not close shipping loop: %#v", ready)
			}
			if withContract && (!strings.Contains(message, "deploy-it contract") || !strings.Contains(message, "already trusted")) {
				t.Fatalf("contract handoff guidance = %#v", ready)
			}
			if !withContract && !strings.Contains(message, "deployment is intentionally skipped") {
				t.Fatalf("absent-contract guidance = %#v", ready)
			}
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
			hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_response": map[string]any{"exit_code": 0}})
			done := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "cwd": repo, "hook_event_name": "Stop"})
			doneMessage := done["systemMessage"].(string)
			if withContract && (!strings.Contains(doneMessage, "shipping completed") || !strings.Contains(doneMessage, "Confirm the ship-it deploy-it handoff")) {
				t.Fatalf("successful shipping did not preserve deploy proof boundary: %#v", done)
			}
			if !withContract && !strings.Contains(doneMessage, "shipping completed") {
				t.Fatalf("successful shipping not recorded: %#v", done)
			}
		})
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
	if !strings.Contains(message, "NOT VERIFIED") || strings.Contains(message, "ship-ready") || strings.Contains(message, "Run ship-it") {
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
			shipReady := strings.Contains(out["systemMessage"].(string), "ship-ready")
			if shipReady != test.wantShipReady {
				t.Fatalf("newest revision result was overwritten: ready=%v output=%#v", shipReady, out)
			}
		})
	}
}

func TestFailedShipOrDeployIsNotRetriedByClosingLoop(t *testing.T) {
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
			if !strings.Contains(message, "did not complete") || strings.Contains(message, "Run ship-it") {
				t.Fatalf("failed delivery was treated as retryable: %#v", out)
			}
		})
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
	text := strings.ToLower(string(mustJSON(out)))
	if !strings.Contains(text, "gentle note") || strings.Contains(text, "clear steer") {
		t.Fatalf("progress did not reset passive-wait tone: %#v", out)
	}
}

func TestExternalMutationGetsTargetCheckWithoutBlocking(t *testing.T) {
	for i, command := range []string{"ship-it", "deploy-it production", "curl -X DELETE https://vault.example/items/1"} {
		dir := t.TempDir()
		out := hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": fmt.Sprintf("target-%d", i), "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": "action", "tool_input": map[string]any{"command": command},
		})
		text := string(mustJSON(out))
		if !strings.Contains(text, "advisory, never a gate") || !strings.Contains(text, "If they match, execute now") {
			t.Fatalf("missing target coaching for %q: %#v", command, out)
		}
		if strings.Contains(text, `"decision":"deny"`) || strings.Contains(text, `"decision":"block"`) {
			t.Fatalf("target coaching blocked %q: %#v", command, out)
		}
	}
}

func TestDuplicateSubagentAndFailedCheckEditsGetAdvisoryCoaching(t *testing.T) {
	dir := t.TempDir()
	input := map[string]any{"agent_type": "explorer", "task_name": "scan", "message": "scan repo"}
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "workers", "hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "tool_use_id": "one", "tool_input": input})
	duplicate := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "workers", "hook_event_name": "PreToolUse", "tool_name": "spawn_agent", "tool_use_id": "two", "tool_input": input})
	if text := string(mustJSON(duplicate)); !strings.Contains(text, "parallel subagents are encouraged") || !strings.Contains(text, "identical assignment is redundant") || strings.Contains(text, `"decision":"block"`) {
		t.Fatalf("duplicate worker coaching = %#v", duplicate)
	}

	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 1}})
	edit := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failure", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "fix"}})
	if text := string(mustJSON(edit)); !strings.Contains(text, "Failure containment") || !strings.Contains(text, "failing check") {
		t.Fatalf("failed-check edit coaching = %#v", edit)
	}
}

func TestActiveGoalIsNotReportedComplete(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "start", "hook_event_name": "PreToolUse", "tool_name": "functions.create_goal", "tool_use_id": "create", "tool_input": map[string]any{"objective": "finish"}})
	hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "start", "hook_event_name": "PostToolUse", "tool_name": "functions.create_goal", "tool_use_id": "create", "tool_response": map[string]any{"goal": map[string]any{"status": "active"}}})
	out := hook(t, dir, map[string]any{"session_id": "goal", "turn_id": "later", "hook_event_name": "Stop"})
	message, _ := out["systemMessage"].(string)
	if !strings.Contains(message, "Goal result: ACTIVE") || strings.Contains(message, "Goal result: SUCCESS") || strings.Contains(message, "Closing loop:") {
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
	for _, want := range []string{"paused objective", "Call get_goal first", "call create_goal with that exact objective", "does not change Codex goal state"} {
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
	if !strings.Contains(string(mustJSON(active)), "Goal active") {
		t.Fatalf("active account goal was not recognized: %#v", active)
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
	if !strings.Contains(string(mustJSON(out)), "Goal active") {
		t.Fatalf("active native goal was not recognized: %#v", out)
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
	verified := state{Revision: 1, VerifiedRevision: 1, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true, TodosParked: 2}
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
	if s.TodosParked != 1 || s.TodosCompleted != 1 || !strings.Contains(reportLine(s), "Deferred work: 1 parked, 1 completed") {
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
