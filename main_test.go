package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	if s.Tests != 1 || s.TestPasses != 1 || s.TotalTestMillis != 12_500 || grade(s) != "B" || numericScore(s) != 100 {
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

func TestDeniedCombinedTestAndProductionIsNotCountedAsTest(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "combined", "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "combined", "tool_input": map[string]any{"command": "go test ./... && git push origin main"}})
	if !strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
		t.Fatalf("combined unverified production command was allowed: %#v", out)
	}
	s := loadTestState(t, dir)
	if s.Tests != 0 || len(s.Pending) != 0 || s.ProductionBlocks != 1 {
		t.Fatalf("denied command changed executed-test state: %#v", s)
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
	if !strings.Contains(reportLine(unverified), "Final result: FAIL") {
		t.Fatalf("unverified edit reported PASS: %s", reportLine(unverified))
	}
	verified := state{Revision: 1, VerifiedRevision: 1, Tests: 2, TestPasses: 2, LastTestPassed: true, LastTestResultKnown: true}
	if grade(verified) != "A" || numericScore(verified) != 100 {
		t.Fatalf("verified efficient run not rewarded: grade=%s score=%d", grade(verified), numericScore(verified))
	}
}

func TestVerifiedSmsbridgeScaleRunIsDNotF(t *testing.T) {
	s := state{
		TotalCalls: 80, CallCostUnits: 320, Tests: 2, TestPasses: 2,
		Revision: 14, VerifiedRevision: 14, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 1, ProductionBlocks: 4, MaxInspectionStreak: 10, PassiveWaits: 1,
		TotalTestMillis: 11_721, MaxTestMillis: 11_640,
	}
	if score, gotGrade := numericScore(s), grade(s); score != 58 || gotGrade != "D" {
		t.Fatalf("verified high-cost run = %s %d, want D 58", gotGrade, score)
	}
	if report := reportLine(s); !strings.Contains(report, "Final result: PASS") || !strings.Contains(report, "Discipline score: D (58/100)") {
		t.Fatalf("verified run report contradicts completion: %s", report)
	}
}

func TestVerifiedDeliveredBetterArgoScaleRunIsC(t *testing.T) {
	s := state{
		TotalCalls: 73, CallCostUnits: 292, Tests: 5, TestPasses: 4,
		Revision: 15, VerifiedRevision: 15, LastTestPassed: true, LastTestResultKnown: true,
		RepeatedWarnings: 2, ProductionBlocks: 3, ProductionCompletions: 1, PassiveWaits: 1,
		TotalTestMillis: 3_004, MaxTestMillis: 985, RedundantTestMillis: 2_099,
	}
	if score, gotGrade := numericScore(s), grade(s); score != 67 || gotGrade != "C" {
		t.Fatalf("verified delivered run = %s %d, want C 67", gotGrade, score)
	}
}

func TestCorrectnessFailureStillGetsF(t *testing.T) {
	for _, s := range []state{
		{Revision: 1},
		{Revision: 1, Tests: 1, TestFailures: 1, LastTestResultKnown: true},
		{OpenDeliveryContractFailure: true},
	} {
		if finalPassed(s) || grade(s) != "F" || !strings.Contains(reportLine(s), "Final result: FAIL") {
			t.Fatalf("true failure escaped F: %#v report=%s", s, reportLine(s))
		}
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

func TestVersionCreditsColinKnapp(t *testing.T) {
	var out bytes.Buffer
	printVersion(&out)
	for _, want := range []string{"one-shot-tally 1.8.1", "ColinKnapp.com"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("version missing %q: %s", want, out.String())
		}
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
	if s.ProductionCompletions != 1 || !strings.Contains(reportLine(s), "Production: 0 blocked, 1 completed") {
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
	if !strings.Contains(string(mustJSON(wait)), "passive waiting or polling") {
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
	if !strings.Contains(string(mustJSON(out)), "without a one-shot-tally background record") {
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

func TestRemovingDeliveryContractIsDeniedAndFailsRun(t *testing.T) {
	dir := t.TempDir()
	patch := "*** Begin Patch\n*** Delete File: .woodpecker.yml\n*** Update File: README.md\n@@\n-Pushing main triggers .woodpecker.yml, which deploys after convergence.\n+Run deployment commands manually.\n*** End Patch"
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "contract", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "delete", "tool_input": map[string]any{"patch": patch}})
	if !strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) || !strings.Contains(string(mustJSON(out)), "automated deploy gate") {
		t.Fatalf("contract deletion not denied: %#v", out)
	}
	s := loadTestState(t, dir)
	if s.DeliveryContractFailures != 1 || !s.OpenDeliveryContractFailure || s.Revision != 0 || numericScore(s) != 0 || grade(s) != "F" || !strings.Contains(reportLine(s), "Final result: FAIL") {
		t.Fatalf("contract failure not durable: %#v report=%s", s, reportLine(s))
	}
	if !strings.Contains(string(mustJSON(out)), "Continue now with a corrected") {
		t.Fatalf("denial encourages stopping instead of recovery: %#v", out)
	}
}

func TestSamePatchDeliveryMigrationIsAllowed(t *testing.T) {
	dir := t.TempDir()
	patch := "*** Begin Patch\n*** Delete File: .woodpecker.yml\n*** Update File: README.md\n@@\n-Ship with ./ship.sh; Woodpecker deploys after convergence.\n+Ship with ship-it, then deploy with deploy-it after convergence.\n*** End Patch"
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "migration", "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "replace", "tool_input": map[string]any{"patch": patch}})
	if strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) {
		t.Fatalf("equivalent migration denied: %#v", out)
	}
	if s := loadTestState(t, dir); s.DeliveryContractFailures != 0 {
		t.Fatalf("migration marked failed: %#v", s)
	}
}

func TestDirectDeliveryEntrypointRemovalIsDenied(t *testing.T) {
	for _, command := range []string{"rm scripts/ship.sh", "git rm -- deploy.sh", "rm .woodpecker.yml", "git rm .github/workflows/deploy.yml"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "delete", "tool_input": map[string]any{"command": command}})
			if !strings.Contains(string(mustJSON(out)), `"permissionDecision":"deny"`) || numericScore(loadTestState(t, dir)) != 0 {
				t.Fatalf("direct removal not failed: %#v", out)
			}
		})
	}
}

func TestCorrectedDeliveryContractEditRecoversAndCanShip(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "recovered-contract"}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "bad", "tool_input": map[string]any{"patch": "*** Begin Patch\n*** Update File: AGENTS.md\n@@\n-Run ship-it after verification.\n+Commit however is easiest.\n*** End Patch"}})
	corrected := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "apply_patch", "tool_use_id": "corrected", "tool_input": map[string]any{"patch": "*** Begin Patch\n*** Update File: README.md\n@@\n-old text\n+new useful text; ship-it remains required\n*** End Patch"}})
	if strings.Contains(string(mustJSON(corrected)), `"permissionDecision":"deny"`) {
		t.Fatalf("corrected edit denied: %#v", corrected)
	}
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "apply_patch", "tool_use_id": "corrected", "tool_response": map[string]any{"success": true}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."}})
	hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0}})
	ship := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "ship", "tool_input": map[string]any{"command": "ship-it"}})
	if strings.Contains(string(mustJSON(ship)), `"permissionDecision":"deny"`) {
		t.Fatalf("recovered verified turn could not ship: %#v", ship)
	}
	s := loadTestState(t, dir)
	if s.DeliveryContractRecoveries != 1 || s.OpenDeliveryContractFailure || numericScore(s) != 77 || !strings.Contains(reportLine(s), "Final result: PASS") {
		t.Fatalf("recovery state = %#v score=%d report=%s", s, numericScore(s), reportLine(s))
	}
}

func TestSessionGuidanceValuesCompletionOverScore(t *testing.T) {
	dir := t.TempDir()
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "guidance", "hook_event_name": "SessionStart"})
	text := string(mustJSON(out))
	for _, want := range []string{"Complete the requested outcome", "do not optimize a score by doing nothing", "Spend the time necessary", "recovery remains eligible to ship"} {
		if !strings.Contains(text, want) {
			t.Fatalf("guidance missing %q: %#v", want, out)
		}
	}
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
