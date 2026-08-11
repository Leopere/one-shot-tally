package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestMechanicalTallyAndGrade(t *testing.T) {
	dir := t.TempDir()
	base := map[string]any{"session_id": "s", "turn_id": "t", "hook_event_name": "PreToolUse", "tool_name": "Bash"}
	for i := 1; i <= 3; i++ {
		base["tool_use_id"] = string(rune('a' + i))
		base["tool_input"] = map[string]any{"command": "git status --short"}
		out := hook(t, dir, base)
		if i == 3 && !strings.Contains(string(mustJSON(out)), "received the same input 3 times") {
			t.Fatalf("missing repetition warning: %#v", out)
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
	if s.Tests != 1 || s.TestPasses != 1 || s.TotalTestMillis != 12_500 || grade(s) != "B" || numericScore(s) != 92 {
		t.Fatalf("unexpected state: %#v grade=%s score=%d", s, grade(s), numericScore(s))
	}
}

func TestBlocksUnverifiedProductionButAllowsRequiredSixthTest(t *testing.T) {
	dir := t.TempDir()
	production := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "p", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "p1", "tool_input": map[string]any{"command": "git " + "push origin main"}})
	if !strings.Contains(string(mustJSON(production)), `"permissionDecision":"deny"`) {
		t.Fatalf("production not denied: %#v", production)
	}

	var sixth map[string]any
	for i := 1; i <= 6; i++ {
		sixth = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "six", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": fmt.Sprintf("test-%d", i), "tool_input": map[string]any{"command": "pytest suite"}})
	}
	encoded := string(mustJSON(sixth))
	if strings.Contains(encoded, `"permissionDecision":"deny"`) {
		t.Fatalf("required sixth test was denied: %#v", sixth)
	}
	if !strings.Contains(encoded, "exceeds the ordinary 5-run guideline") {
		t.Fatalf("sixth test lacks pacing warning: %#v", sixth)
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
	if !strings.Contains(string(mustJSON(production)), `"permissionDecision":"deny"`) {
		t.Fatalf("fake test unlocked production: %#v", production)
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

func TestInspectionWarningGivesStateAwareNextAction(t *testing.T) {
	dir := t.TempDir()
	var out map[string]any
	for i := 0; i < 8; i++ {
		out = hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "advice", "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": fmt.Sprintf("read-%d", i),
			"tool_input": map[string]any{"command": fmt.Sprintf("git status --short %d", i)},
		})
	}
	encoded := string(mustJSON(out))
	if !strings.Contains(encoded, "8 consecutive inspections with 0 edits and 0 tests") {
		t.Fatalf("warning lacks observed state: %#v", out)
	}
	if !strings.Contains(encoded, "choose the highest-confidence change, and make one coherent edit") {
		t.Fatalf("warning lacks actionable recommendation: %#v", out)
	}
}

func TestStopRequestsMechanicalLineOnce(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "e", "tool_input": map[string]any{"command": "patch"}})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "last_assistant_message": "Done"})
	if out["decision"] != "block" || !strings.Contains(out["reason"].(string), "Tool calls:") {
		t.Fatalf("unexpected stop output: %#v", out)
	}
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stop", "hook_event_name": "Stop", "stop_hook_active": true, "last_assistant_message": "Done"})
	if out["decision"] == "block" {
		t.Fatalf("stop looped: %#v", out)
	}
	life, err := loadLifetime()
	if err != nil {
		t.Fatal(err)
	}
	if life.Runs != 1 || life.AverageScore != 25 || life.Grades["F"] != 1 {
		t.Fatalf("unexpected lifetime: %#v", life)
	}
}

func TestRequiredTestingOutweighsEfficiency(t *testing.T) {
	unverified := state{Revision: 1}
	if grade(unverified) != "F" || numericScore(unverified) != 25 {
		t.Fatalf("unverified edit rewarded: grade=%s score=%d", grade(unverified), numericScore(unverified))
	}
	verified := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
	if grade(verified) != "A" || numericScore(verified) != 100 {
		t.Fatalf("verified efficient run not rewarded: grade=%s score=%d", grade(verified), numericScore(verified))
	}
}

func TestLongRedundantTestChainsLoseScore(t *testing.T) {
	efficient := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, TotalTestMillis: 240_000, MaxTestMillis: 180_000}
	if numericScore(efficient) != 100 || grade(efficient) != "A" {
		t.Fatalf("necessary test time penalized too early: %d %s", numericScore(efficient), grade(efficient))
	}
	redundant := efficient
	redundant.Tests = 4
	redundant.TotalTestMillis = 840_000
	redundant.RedundantTestMillis = 600_000
	if numericScore(redundant) >= 70 || grade(redundant) == "A" || grade(redundant) == "B" {
		t.Fatalf("redundant chain insufficiently penalized: %d %s", numericScore(redundant), grade(redundant))
	}
}

func TestDurationComesFromActualToolResponse(t *testing.T) {
	raw := json.RawMessage(`{"result":{"wall_time_seconds":61.25}}`)
	if got := testDurationMillis(raw, time.Time{}); got != 61_250 {
		t.Fatalf("duration = %d", got)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestSparkCallsAreEncouragedAndDiscounted(t *testing.T) {
	dir := t.TempDir()
	start := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "spark", "hook_event_name": "SessionStart"})
	if !strings.Contains(string(mustJSON(start)), "spark_worker") {
		t.Fatalf("session guidance does not encourage Spark: %#v", start)
	}
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

func TestHelpDocumentsSparkPolicy(t *testing.T) {
	var out bytes.Buffer
	printHelp(&out)
	for _, want := range []string{"status [--json]", "spark_worker", "0.25 normal calls", "correctness gates are never discounted"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q: %s", want, out.String())
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
	if s.Tests != 0 || s.ProductionBlocks != 0 || s.Revision != 1 {
		t.Fatalf("patch contents treated as shell commands: %#v", s)
	}
}

func TestProductionBlockCanRecoverAfterFinalVerification(t *testing.T) {
	s := state{
		Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2,
		LastTestPassed: true, LastTestResultKnown: true, ProductionBlocks: 1,
	}
	if got := numericScore(s); got != 85 {
		t.Fatalf("recovered production block score = %d, want 85", got)
	}
	if got := grade(s); got != "B" {
		t.Fatalf("recovered production block grade = %s, want B", got)
	}
}
