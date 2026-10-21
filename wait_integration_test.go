package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestWaitHooksScoreAndAdviseWithoutChangingOutcome(t *testing.T) {
	dir := retainedTestDir(t)
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("wait-%d", i)
		hook(t, dir, map[string]any{"session_id": "waits", "turn_id": "polling", "hook_event_name": "PreToolUse", "tool_name": "functions.wait", "tool_use_id": id, "tool_input": map[string]any{"cell_id": "running", "yield_time_ms": 1000}})
		out := hook(t, dir, map[string]any{"session_id": "waits", "turn_id": "polling", "hook_event_name": "PostToolUse", "tool_name": "functions.wait", "tool_use_id": id, "tool_response": map[string]any{"wall_time_seconds": 1}})
		text := hookAdditionalContext(out)
		wantNotice := i == 3 || i == 7
		if (text != "") != wantNotice || out["decision"] == "block" {
			t.Fatalf("wait %d advisory was missing, repeated, or blocking: %#v", i, out)
		}
		if wantNotice && (!strings.Contains(text, "TODO check-in") || !strings.Contains(text, "unblock")) {
			t.Fatalf("wait advisory lacks a recovery action: %q", text)
		}
	}
	s := loadTestState(t, dir)
	if s.WaitCalls != 8 || s.TotalWaitMillis != 8000 || s.LongWaits != 0 || s.WaitFeedbackLevel != 2 || numericScore(s) != 70 || recordedOutcome(s) != outcomeActivity {
		t.Fatalf("incorrect wait accounting: %#v", s)
	}
	if !strings.Contains(reportLine(s), "Waits: 8 calls;") {
		t.Fatal("status omitted the wait evidence")
	}
}

func TestLinkedExecutionPollsUseEachPollDurationOnce(t *testing.T) {
	dir := retainedTestDir(t)
	pre := func(id, tool string, input any) {
		t.Helper()
		hook(t, dir, map[string]any{"session_id": "polls", "turn_id": "command", "hook_event_name": "PreToolUse", "tool_name": tool, "tool_use_id": id, "tool_input": input})
	}
	post := func(id, tool string, seconds float64, running bool) map[string]any {
		t.Helper()
		response := map[string]any{"chunk_id": id, "wall_time_seconds": seconds, "output": "fixture"}
		if running {
			response["session_id"] = 42
		} else {
			response["exit_code"] = 0
		}
		return hook(t, dir, map[string]any{"session_id": "polls", "turn_id": "command", "hook_event_name": "PostToolUse", "tool_name": tool, "tool_use_id": id, "tool_response": response})
	}
	pre("command", "exec_command", map[string]any{"cmd": "go test ./..."})
	post("command", "exec_command", 1, true)
	pre("poll-one", "write_stdin", map[string]any{"session_id": 42, "chars": ""})
	first := post("poll-one", "write_stdin", 12, true)
	s := loadTestState(t, dir)
	if s.WaitCalls != 1 || s.TotalWaitMillis != 12_000 || s.ExcessWaitMillis != 2000 || s.Running["42"].Wait || !s.Running["42"].WaitStartedAt.IsZero() {
		t.Fatalf("first poll lost linkage or retained its wait timer: %#v", s)
	}
	if !strings.Contains(hookAdditionalContext(first), "TODO check-in") {
		t.Fatal("long running poll omitted check-in guidance")
	}
	pre("poll-two", "write_stdin", map[string]any{"session_id": 42})
	post("poll-two", "write_stdin", 3, false)
	s = loadTestState(t, dir)
	if s.WaitCalls != 2 || s.TotalWaitMillis != 15_000 || s.LongWaits != 1 || s.Tests != 1 || s.TestPasses != 1 || len(s.Running) != 0 {
		t.Fatalf("poll time or original check counted twice: %#v", s)
	}
}

func TestWaitPenaltySurvivesBonusesAndVerifiedFloor(t *testing.T) {
	s := state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true, WaitCalls: 40, BackgroundRecords: 2, TodosParked: 2, TodosCompleted: 2}
	if recordedOutcome(s) != outcomeVerified || outcomeScore(s) != 100 || numericScore(s) != 30 {
		t.Fatalf("wait penalty was erased or changed verification: outcome=%s score=%d", recordedOutcome(s), numericScore(s))
	}
	s.PassiveWaits, s.WaitCalls = 3, 3
	if numericScore(s) != 79 {
		t.Fatalf("bonuses erased legacy passive waiting: %d", numericScore(s))
	}
	s.PassiveWaits, s.WaitCalls = 0, 40
	// Existing diagnostics can already reach the verified-work floor. Severe
	// waiting still subtracts its additional penalty from that floor.
	s.Tests, s.TestPasses = 20, 20
	if numericScore(s) != 0 || recordedOutcome(s) != outcomeVerified {
		t.Fatalf("verified floor hid severe waiting: %d", numericScore(s))
	}
	s.VerifiedRevision = 0
	if numericScore(s) != 0 {
		t.Fatal("unverified-work shortcut hid the wait penalty")
	}
}

func TestActiveStdinAndWaitNamedWorkAreNotPenalized(t *testing.T) {
	dir := retainedTestDir(t)
	for i, call := range []struct {
		tool  string
		input any
	}{
		{"functions.write_stdin", map[string]any{"session_id": 123, "chars": " "}},
		{"functions.write_stdin", map[string]any{"session_id": 123, "chars": "\n"}},
		{"wait_for_build_configuration", map[string]any{}},
	} {
		hook(t, dir, map[string]any{"session_id": "active", "turn_id": "stdin", "hook_event_name": "PreToolUse", "tool_name": call.tool, "tool_use_id": fmt.Sprint(i), "tool_input": call.input})
	}
	s := loadTestState(t, dir)
	if s.WaitCalls != 0 || s.PassiveWaits != 0 || numericScore(s) != 100 {
		t.Fatalf("active input or ordinary work penalized: %#v", s)
	}
}
