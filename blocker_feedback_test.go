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
)

func feedbackEvent(t *testing.T, dir, cwd, turn, name string, extra map[string]any) map[string]any {
	t.Helper()
	e := map[string]any{"session_id": "blocker-session", "turn_id": turn, "cwd": cwd, "hook_event_name": name}
	for key, value := range extra {
		e[key] = value
	}
	return hook(t, dir, e)
}

func receiptEvent(t *testing.T, dir, cwd, turn, attempt, status string) map[string]any {
	t.Helper()
	exitCode := 0
	if status == "failed" {
		exitCode = 1
	}
	commit, _ := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	return feedbackEvent(t, dir, cwd, turn, "DeliveryResult", map[string]any{"delivery_kind": "ship", "delivery_status": status, "delivery_attempt_id": attempt, "delivery_commit": strings.TrimSpace(string(commit)), "tool_response": map[string]any{"exit_code": exitCode}})
}

func testReceiptID(sequence int) string { return fmt.Sprintf("%d-%024x", sequence, sequence) }

func TestTerminalDeploymentAbandonmentFailsAndChallengesOnce(t *testing.T) {
	dir, cwd := retainedTestDir(t), retainedTestDir(t)
	message := "• It wasn’t deployed. I fixed the invalid deployment timeout and failing registry publication check; regression tests and security checks passed."
	out := feedbackEvent(t, dir, cwd, "first", "Stop", map[string]any{"last_assistant_message": message})
	if out["decision"] != "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: FAILED") || !strings.Contains(out["systemMessage"].(string), "Outcome grade: 0/100") {
		t.Fatalf("abandonment escaped: %#v", out)
	}
	if !strings.Contains(out["reason"].(string), "Fact-check") {
		t.Fatal("missing blocker challenge")
	}
	out = feedbackEvent(t, dir, cwd, "next", "Stop", map[string]any{"last_assistant_message": message})
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "FAILED") {
		t.Fatalf("repeated blocker must stay failed without loop: %#v", out)
	}
	l, _ := ledgerLoad(event{SessionID: "blocker-session", CWD: cwd})
	if l.snapshot().Unsupported != 1 {
		t.Fatalf("claim not distinguished: %#v", l)
	}
}

func TestNativeFailureSurvivesChecksAndRecoveryIsVerified(t *testing.T) {
	dir := retainedTestDir(t)
	cwd, _ := committedTestRepo(t, "package app\n")
	out := receiptEvent(t, dir, cwd, "failed-turn", testReceiptID(1), "failed")
	if out["decision"] != "block" {
		t.Fatalf("failure did not request review: %#v", out)
	}
	feedbackEvent(t, dir, cwd, "repair-turn", "PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "check", "tool_input": map[string]any{"command": "go test ./..."}})
	feedbackEvent(t, dir, cwd, "repair-turn", "PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "check", "tool_response": map[string]any{"exit_code": 0}})
	out = feedbackEvent(t, dir, cwd, "repair-turn", "Stop", map[string]any{"last_assistant_message": "Tests passed. Deployment is fixed."})
	if !strings.Contains(out["systemMessage"].(string), "FAILED") {
		t.Fatalf("check or prose cleared delivery: %#v", out)
	}
	out = receiptEvent(t, dir, cwd, "repair-turn", testReceiptID(2), "succeeded")
	if out["decision"] == "block" || !strings.Contains(out["systemMessage"].(string), "Recorded outcome: RECOVERED") || !strings.Contains(out["systemMessage"].(string), "INCREDIBLE WIN") || !strings.Contains(out["systemMessage"].(string), "Outcome grade: 100/100") {
		t.Fatalf("recovery not credited: %#v", out)
	}
	assertEmptyHookOutput(t, receiptEvent(t, dir, cwd, "repair-turn", testReceiptID(2), "succeeded"))
	l, _ := ledgerLoad(event{SessionID: "blocker-session", CWD: cwd})
	if l.Recoveries != 1 || l.unresolvedCount() != 0 {
		t.Fatalf("duplicate receipt changed recovery: %#v", l)
	}
}

func TestClaimOnlyCannotEarnRecoveryAndAuditDoesNotDemandDeployment(t *testing.T) {
	dir := retainedTestDir(t)
	cwd, _ := committedTestRepo(t, "package app\n")
	feedbackEvent(t, dir, cwd, "audit", "UserPromptSubmit", map[string]any{"prompt": "Read-only audit. Do not deploy."})
	out := feedbackEvent(t, dir, cwd, "audit", "Stop", map[string]any{"last_assistant_message": "It wasn’t deployed."})
	if out["decision"] == "block" || strings.Contains(out["systemMessage"].(string), "FAILED") {
		t.Fatalf("audit incorrectly failed: %#v", out)
	}
	feedbackEvent(t, dir, cwd, "deploy", "Stop", map[string]any{"last_assistant_message": "It wasn’t deployed."})
	out = receiptEvent(t, dir, cwd, "deploy", testReceiptID(1), "succeeded")
	if strings.Contains(out["systemMessage"].(string), "INCREDIBLE WIN") {
		t.Fatal("unverified claim earned recovery credit")
	}
}

func TestTerminalBlockerAvoidsQuotesAndHistoricalFailures(t *testing.T) {
	for _, message := range []string{`> It wasn't deployed.`, `"It wasn't deployed." is bad phrasing.`, "```\nIt wasn't deployed.\n```", "Deployment failed earlier; deployment succeeded after retry.", "I couldn't complete it initially, but recovered."} {
		if got := terminalBlocker(message); got != "" {
			t.Errorf("%q became %s", message, got)
		}
	}
	for _, message := range []string{"It wasn’t deployed.", "Production deployment remains blocked by an empty stale lock.", "Deployment failed.", "I'm blocked.", "Seven local files remain unpublished.", "Production is seven commits behind GitHub."} {
		if got := terminalBlocker(message); got == "" {
			t.Errorf("missed %q", message)
		}
	}
}

func TestNativeSuccessCannotClearNewLocalWork(t *testing.T) {
	dir := retainedTestDir(t)
	cwd, _ := committedTestRepo(t, "package app\n")
	receiptEvent(t, dir, cwd, "failed", testReceiptID(1), "failed")
	if err := os.WriteFile(filepath.Join(cwd, "new-local.txt"), []byte("unpublished\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out := receiptEvent(t, dir, cwd, "repair", testReceiptID(2), "succeeded")
	message := out["systemMessage"].(string)
	if !strings.Contains(message, "Recorded outcome: FAILED") || strings.Contains(message, "INCREDIBLE WIN") || !strings.Contains(message, "local delivery remains unverified") {
		t.Fatalf("old receipt cleared newer work: %#v", out)
	}
}

func TestShippingClaimClearsWithShippingReceipt(t *testing.T) {
	dir := retainedTestDir(t)
	cwd, _ := committedTestRepo(t, "package app\n")
	for _, message := range []string{"It wasn't shipped.", "Shipping failed.", "Seven local files remain unpublished."} {
		if got := terminalBlocker(message); got != "ship" {
			t.Fatalf("%q classified %q", message, got)
		}
	}
	feedbackEvent(t, dir, cwd, "ship", "Stop", map[string]any{"last_assistant_message": "It wasn't shipped."})
	commit, _ := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	out := feedbackEvent(t, dir, cwd, "ship", "DeliveryResult", map[string]any{"delivery_kind": "ship", "delivery_status": "succeeded", "delivery_attempt_id": testReceiptID(1), "delivery_commit": strings.TrimSpace(string(commit)), "tool_response": map[string]any{"exit_code": 0}})
	if strings.Contains(out["systemMessage"].(string), "FAILED") {
		t.Fatalf("ship receipt did not clear shipping claim: %#v", out)
	}
}

func TestNativeStdoutOnlyDoesNotProveCheckSuccess(t *testing.T) {
	dir, cwd := retainedTestDir(t), retainedTestDir(t)
	feedbackEvent(t, dir, cwd, "stdout-only", "PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "check", "tool_input": map[string]any{"command": "go test ./..."}})
	feedbackEvent(t, dir, cwd, "stdout-only", "PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "check", "tool_response": "PASS\nok example\n"})
	out := feedbackEvent(t, dir, cwd, "stdout-only", "Stop", nil)
	message := out["systemMessage"].(string)
	if !strings.Contains(message, "0 passed") || !strings.Contains(message, "without a verified exit status") {
		t.Fatalf("stdout mistaken for verified check: %#v", out)
	}
}

func TestOlderNativeSuccessCannotClearNewerFailure(t *testing.T) {
	dir := retainedTestDir(t)
	cwd, _ := committedTestRepo(t, "package app\n")
	receiptEvent(t, dir, cwd, "newer", testReceiptID(200), "failed")
	for _, invalid := range []string{"missing-timestamp", "0-000000000000000000000000", "999999999999999999999999-000000000000000000000000"} {
		var out bytes.Buffer
		if err := deliveryResult(event{HookEventName: "DeliveryResult", SessionID: "blocker-session", TurnID: "invalid", CWD: cwd, DeliveryKind: "ship", DeliveryStatus: "failed", DeliveryAttemptID: invalid, ToolResponse: json.RawMessage(`{"exit_code":1}`)}, &out); err == nil {
			t.Fatalf("invalid receipt accepted: %q", invalid)
		}
	}
	out := receiptEvent(t, dir, cwd, "older", testReceiptID(100), "succeeded")
	if !strings.Contains(out["systemMessage"].(string), "newer delivery result") {
		t.Fatalf("old success was accepted: %#v", out)
	}
	l, _ := ledgerLoad(event{SessionID: "blocker-session", CWD: cwd})
	if l.unresolvedCount() != 1 || l.Recoveries != 0 {
		t.Fatalf("late success erased failure: %#v", l)
	}
}

func TestLegacyDeployReceiptCannotCountAsDelivery(t *testing.T) {
	cwd, _ := committedTestRepo(t, "package app\n")
	commit, _ := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	var output bytes.Buffer
	err := deliveryResult(event{
		HookEventName: "DeliveryResult", SessionID: "blocker-session", TurnID: "legacy-deploy",
		CWD: cwd, DeliveryKind: "deploy", DeliveryStatus: "succeeded", DeliveryAttemptID: testReceiptID(1),
		DeliveryCommit: strings.TrimSpace(string(commit)), ToolResponse: json.RawMessage(`{"exit_code":0}`),
	}, &output)
	if err == nil {
		t.Fatal("legacy deploy receipt was accepted as delivery evidence")
	}
}

func TestAsyncCheckResultFollowsRunningExecution(t *testing.T) {
	dir, cwd := retainedTestDir(t), retainedTestDir(t)
	feedbackEvent(t, dir, cwd, "async", "PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "start", "tool_input": map[string]any{"command": "go test ./..."}})
	running := map[string]any{"chunk_id": "a", "session_id": 123, "output": "", "wall_time_seconds": 1}
	feedbackEvent(t, dir, cwd, "async", "PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": "start", "tool_response": running})
	feedbackEvent(t, dir, cwd, "async", "PreToolUse", map[string]any{"tool_name": "functions.write_stdin", "tool_use_id": "wait", "tool_input": map[string]any{"session_id": 123}})
	data, _ := json.Marshal(map[string]any{"chunk_id": "b", "exit_code": 1, "output": "failed", "wall_time_seconds": 2})
	feedbackEvent(t, dir, cwd, "async", "PostToolUse", map[string]any{"tool_name": "functions.write_stdin", "tool_use_id": "wait", "tool_response": []map[string]any{{"type": "input_text", "text": string(data)}}})
	s := loadTestState(t, dir)
	if s.Tests != 1 || s.TestFailures != 1 || len(s.Running) != 0 || len(s.Pending) != 0 {
		t.Fatalf("lost asynccheck: %#v", s)
	}
}

func TestUnrelatedCommandCannotClearClaimedFailedOperation(t *testing.T) {
	dir, cwd := retainedTestDir(t), retainedTestDir(t)
	command := func(id, text string, code int) {
		feedbackEvent(t, dir, cwd, "work", "PreToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": id, "tool_input": map[string]any{"command": text}})
		feedbackEvent(t, dir, cwd, "work", "PostToolUse", map[string]any{"tool_name": "Bash", "tool_use_id": id, "tool_response": map[string]any{"exit_code": code}})
	}
	command("fail", "make verify", 1)
	feedbackEvent(t, dir, cwd, "work", "Stop", map[string]any{"last_assistant_message": "I'm blocked."})
	command("unrelated", "echo ready", 0)
	out := feedbackEvent(t, dir, cwd, "work", "Stop", nil)
	if !strings.Contains(out["systemMessage"].(string), "FAILED") {
		t.Fatal("unrelated command cleared blocker")
	}
	command("retry", "make verify", 0)
	l, _ := ledgerLoad(event{SessionID: "blocker-session", CWD: cwd})
	if l.unresolvedCount() != 0 || l.Recoveries != 1 {
		t.Fatalf("same operation did not recover: %#v", l)
	}
}
