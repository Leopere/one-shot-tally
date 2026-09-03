package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitResponseResult(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want struct {
			known bool
			pass  bool
		}
	}{
		{name: "explicit_exit_code_zero", raw: `{"result":{"exit_code":0}}`, want: struct{ known, pass bool }{true, true}},
		{name: "nested_exit_code_one", raw: `{"result":{"metadata":{"exit_code":1}}}`, want: struct{ known, pass bool }{true, false}},
		{name: "transport_success_true_unknown", raw: `{"success":true}`, want: struct{ known, pass bool }{false, false}},
		{name: "explicit_success_false", raw: `{"success":false}`, want: struct{ known, pass bool }{true, false}},
		{name: "explicit_error_nonempty", raw: `{"error":"something failed"}`, want: struct{ known, pass bool }{true, false}},
		{name: "nonempty_stderr_unknown", raw: `{"stderr":"npm ERR! test failed"}`, want: struct{ known, pass bool }{false, false}},
		{name: "empty_unknown", raw: ``, want: struct{ known, pass bool }{false, false}},
		{name: "malformed_unknown", raw: `{"result":`, want: struct{ known, pass bool }{false, false}},
		{name: "output_text_only_unknown", raw: `{"output":"FAIL: expected output mismatch"}`, want: struct{ known, pass bool }{false, false}},
		{name: "action_fail_json_text_unknown", raw: `{"output":"{\"Action\":\"fail\",\"Package\":\"x\"}"}`, want: struct{ known, pass bool }{false, false}},
		{name: "serialized_command_result_unknown", raw: `{"output":[{"type":"input_text","text":"{\"exit_code\":0,\"output\":\"ok\"}"}]}`, want: struct{ known, pass bool }{false, false}},
		{name: "serialized_failure_unknown", raw: `{"output":[{"type":"input_text","text":"{\"exit_code\":2,\"output\":\"bad\"}"}]}`, want: struct{ known, pass bool }{false, false}},
		{name: "top_level_content_blocks_unknown", raw: `[{"type":"input_text","text":"Script completed"},{"type":"input_text","text":"{\"exit_code\":0,\"output\":\"ok\"}"}]`, want: struct{ known, pass bool }{false, false}},
		{name: "top_level_array_exit_unknown", raw: `[{"exit_code":0}]`, want: struct{ known, pass bool }{false, false}},
		{name: "structured_output_payload_unknown", raw: `{"output":{"exit_code":0}}`, want: struct{ known, pass bool }{false, false}},
		{name: "structured_content_payload_unknown", raw: `{"content":[{"exit_code":0}]}`, want: struct{ known, pass bool }{false, false}},
		{name: "human_tool_output_unknown", raw: `{"output":[{"type":"input_text","text":"Script completed with passing tests"}]}`, want: struct{ known, pass bool }{false, false}},
		{name: "transport_success_with_failure_text_unknown", raw: `{"success":true,"output":"FAILED tests/test_api.py"}`, want: struct{ known, pass bool }{false, false}},
		{name: "zero_exit_with_stderr_warning_passes", raw: `{"exit_code":0,"stderr":"warning"}`, want: struct{ known, pass bool }{true, true}},
		{name: "arbitrary_nested_result_fields_unknown", raw: `{"data":{"ok":false,"error":"fixture","exit_code":1}}`, want: struct{ known, pass bool }{false, false}},
		{name: "zero_exit_ignores_payload_fields", raw: `{"exit_code":0,"data":{"ok":false,"error":"fixture","exit_code":1}}`, want: struct{ known, pass bool }{true, true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			known, passed := explicitResponseResult(json.RawMessage(tt.raw))
			if known != tt.want.known || passed != tt.want.pass {
				t.Fatalf("explicitResponseResult(%q) = (%v, %v), want (%v, %v)", tt.name, known, passed, tt.want.known, tt.want.pass)
			}
		})
	}
}

func TestGoalTransitionPassedRequiresMatchingGoalStatus(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		transition string
		want       bool
	}{
		{name: "start_active", raw: `{"goal":{"status":"active"}}`, transition: "start", want: true},
		{name: "finish_complete", raw: `{"structuredContent":{"goal":{"status":"complete"}}}`, transition: "finish", want: true},
		{name: "finish_blocked", raw: `{"goal":{"status":"blocked"}}`, transition: "finish", want: true},
		{name: "wrong_status", raw: `{"goal":{"status":"complete"}}`, transition: "start", want: false},
		{name: "transport_success_only", raw: `{"success":true}`, transition: "start", want: false},
		{name: "unscoped_status", raw: `{"status":"active"}`, transition: "start", want: false},
		{name: "explicit_error_with_status", raw: `{"isError":true,"goal":{"status":"active"}}`, transition: "start", want: false},
		{name: "failed_transport_with_status", raw: `{"success":false,"goal":{"status":"complete"}}`, transition: "finish", want: false},
		{name: "error_text_with_status", raw: `{"error":"write failed","goal":{"status":"blocked"}}`, transition: "finish", want: false},
		{name: "empty_object", raw: `{}`, transition: "start", want: false},
		{name: "malformed", raw: `{"goal":`, transition: "start", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := goalTransitionPassed(json.RawMessage(test.raw), test.transition); got != test.want {
				t.Fatalf("goalTransitionPassed() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestUnknownEditResponseCannotVerifyOrFail(t *testing.T) {
	responses := []any{map[string]any{}, map[string]any{"success": true}, nil}
	for index, response := range responses {
		dir := t.TempDir()
		turn := "unknown-edit-" + string(rune('a'+index))
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": turn, "hook_event_name": "PreToolUse",
			"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
		})
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": turn, "hook_event_name": "PostToolUse",
			"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": response,
		})
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": turn, "hook_event_name": "PreToolUse",
			"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."},
		})
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": turn, "hook_event_name": "PostToolUse",
			"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0},
		})
		out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": turn, "hook_event_name": "Stop"})
		text := string(mustJSON(out))
		if !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") || strings.Contains(text, "Recorded outcome: FAILED") {
			t.Fatalf("unknown edit response %d was guessed: %#v", index, out)
		}
	}
}

func TestSuccessfulNoOpEditDoesNotCreateVerifiedRevision(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "app.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commit := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "no-op-edit", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "cwd": repo, "tool_input": map[string]any{"patch": "no change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "no-op-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "no-op-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "no-op-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "no-op-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("successful no-op edit was treated as a changed revision: %#v", out)
	}
}

func TestObservedWorktreeChangeCanConfirmOpaqueEditResponse(t *testing.T) {
	dir := t.TempDir()
	repo, appPath := committedTestRepo(t, "package app\n\nconst value = \"old\"\n")
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "opaque-edit", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "cwd": repo, "tool_input": map[string]any{"patch": "change"},
	})
	if err := os.WriteFile(appPath, []byte("package app\n\nconst value = \"new\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "opaque-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "cwd": repo, "tool_response": map[string]any{"success": true},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "opaque-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "opaque-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "opaque-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("observed edit plus explicit passing test was not verified: %#v", out)
	}
}

func TestRecordedOutcome(t *testing.T) {
	tests := []struct {
		name     string
		state    state
		expected string
	}{
		{name: "empty_state_no_observed_work", state: state{}, expected: "NO OBSERVED WORK"},
		{name: "total_calls_without_revision_activity", state: state{TotalCalls: 3}, expected: "ACTIVITY OBSERVED"},
		{name: "known_latest_call_failure", state: state{LastTestResultKnown: true, LastTestPassed: false, Tests: 1, TestFailures: 1}, expected: "FAILED"},
		{name: "edited_current_revision_with_passing_test", state: state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true, LastEditResultRevision: 1, VerifiedRevision: 1, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true}, expected: "VERIFIED"},
		{name: "passing_test_before_edit_result_is_activity", state: state{Revision: 1, VerifiedRevision: 1, Tests: 1, TestPasses: 1, LastTestPassed: true, LastTestResultKnown: true}, expected: "ACTIVITY OBSERVED"},
		{name: "unverified_edit", state: state{Revision: 1, LastEditResultKnown: true, LastEditSucceeded: true}, expected: "ACTIVITY OBSERVED"},
		{name: "test_failure", state: state{Revision: 1, VerifiedRevision: 0, Tests: 2, TestPasses: 1, TestFailures: 1, LastTestPassed: false, LastTestResultKnown: true}, expected: "FAILED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recordedOutcome(tt.state); got != tt.expected {
				t.Fatalf("recordedOutcome(%q) = %q, want %q", tt.name, got, tt.expected)
			}
		})
	}
}

func TestUnknownTestResultNeverVerifiesEdit(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "unknown-test", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "unknown-test", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "unknown-test", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "unknown-test", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"success": true, "output": "PASS"},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "unknown-test", "hook_event_name": "Stop"})
	text := string(mustJSON(out))
	if !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") || strings.Contains(text, "ship-ready") {
		t.Fatalf("unknown test result verified edit: %#v", out)
	}
}

func TestKnownFailedCommandReportsFailed(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-command", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "command", "tool_input": map[string]any{"command": "false"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-command", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "command", "tool_response": map[string]any{"exit_code": 1},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failed-command", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: FAILED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("failed command outcome: %#v", out)
	}
}

func TestShellMutationIsActivityEvenAfterPassingCheck(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "edit", "tool_input": map[string]any{"command": "sed -i s/old/new/ app.go"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "shell-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("untracked shell mutation was verified: %#v", out)
	}
}

func TestMixedEditBypassDoesNotRemainVerifiedAfterShellMutation(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	appPath := filepath.Join(repo, "app.go")
	if err := os.WriteFile(appPath, []byte("package app\n\nconst value = \"old\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "app.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commit := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "apply", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "apply", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "shell-mutate", "cwd": repo, "tool_input": map[string]any{"command": "sed -i s/old/new/ app.go"},
	})
	if err := os.WriteFile(appPath, []byte("package app\n\nconst value = \"new\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "shell-mutate", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "Stop"})
	text := string(mustJSON(out))
	if !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("mixed edit path should not stay VERIFIED: %#v", out)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "retest", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "retest", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out = hook(t, dir, map[string]any{"session_id": "s", "turn_id": "mixed-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("observed shell mutation could not be verified by a later test: %#v", out)
	}
}

func TestFailedShellMutationCannotBeRescuedByLaterPassingTest(t *testing.T) {
	dir := t.TempDir()
	repo, appPath := committedTestRepo(t, "package app\n\nconst value = \"old\"\n")
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "initial-test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "initial-test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "shell-edit", "cwd": repo, "tool_input": map[string]any{"command": "sed -i s/old/new/ app.go && false"},
	})
	if err := os.WriteFile(appPath, []byte("package app\n\nconst value = \"new\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "shell-edit", "cwd": repo, "tool_response": map[string]any{"exit_code": 1},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "later-test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "later-test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "failed-shell-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: FAILED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("failed shell mutation was hidden by a later test: %#v", out)
	}
}

func TestShellChainedTestResultIsNotAuthoritative(t *testing.T) {
	for _, command := range []string{"go test ./... || true", "go test ./... && sed -i s/old/new/ app.go"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse",
				"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PostToolUse",
				"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PreToolUse",
				"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": command},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": command, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0},
			})
			out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": command, "hook_event_name": "Stop"})
			if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
				t.Fatalf("ambiguous shell chain verified an edit: %#v", out)
			}
			if state := loadTestState(t, dir); state.Tests != 0 {
				t.Fatalf("ambiguous shell chain counted as an authoritative test: %#v", state)
			}
		})
	}
}

func TestTestStartedBeforeEditCompletionCannotVerify(t *testing.T) {
	dir := t.TempDir()
	common := map[string]any{"session_id": "s", "turn_id": "overlap"}
	hook(t, dir, map[string]any{
		"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": common["session_id"], "turn_id": common["turn_id"], "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("test that overlapped an incomplete edit verified it: %#v", out)
	}
}

func TestNewerTestResultWinsWhenCompletionsAreOutOfOrder(t *testing.T) {
	tests := []struct {
		name      string
		newerExit int
		olderExit int
		want      string
	}{
		{name: "newer_failure", newerExit: 1, olderExit: 0, want: outcomeFailed},
		{name: "newer_pass", newerExit: 0, olderExit: 1, want: outcomeVerified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PreToolUse",
				"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PostToolUse",
				"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
			})
			for _, id := range []string{"older", "newer"} {
				hook(t, dir, map[string]any{
					"session_id": "s", "turn_id": test.name, "hook_event_name": "PreToolUse",
					"tool_name": "Bash", "tool_use_id": id, "tool_input": map[string]any{"command": "go test ./..."},
				})
			}
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "newer", "tool_response": map[string]any{"exit_code": test.newerExit},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "older", "tool_response": map[string]any{"exit_code": test.olderExit},
			})
			if state := loadTestState(t, dir); recordedOutcome(state) != test.want {
				t.Fatalf("completion order overrode invocation order: outcome=%s state=%#v", recordedOutcome(state), state)
			}
		})
	}
}

func TestStaleEditResultCannotOverrideNewerObservedEdit(t *testing.T) {
	dir := t.TempDir()
	repo, appPath := committedTestRepo(t, "package app\n\nconst value = \"old\"\n")
	for _, id := range []string{"older", "newer"} {
		hook(t, dir, map[string]any{
			"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "PreToolUse",
			"tool_name": "apply_patch", "tool_use_id": id, "cwd": repo, "tool_input": map[string]any{"patch": id},
		})
	}
	if err := os.WriteFile(appPath, []byte("package app\n\nconst value = \"newer\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "newer", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "older", "cwd": repo, "tool_response": map[string]any{"exit_code": 1},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "stale-edit", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: VERIFIED") || strings.Contains(text, "Recorded outcome: FAILED") {
		t.Fatalf("stale edit completion overrode newer edit: %#v", out)
	}
}

func TestKnownPreTestSnapshotRequiresKnownPostSnapshot(t *testing.T) {
	dir := t.TempDir()
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	file := filepath.Join(repo, "app.go")
	if err := os.WriteFile(file, []byte("package app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "app.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commit := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "snapshot-loss", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "snapshot-loss", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "snapshot-loss", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_input": map[string]any{"command": "go test ./..."},
	})
	if err := os.Rename(filepath.Join(repo, ".git"), filepath.Join(repo, ".git-hidden")); err != nil {
		t.Fatal(err)
	}
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "snapshot-loss", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "cwd": repo, "tool_response": map[string]any{"exit_code": 0},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "snapshot-loss", "hook_event_name": "Stop"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Recorded outcome: ACTIVITY OBSERVED") || strings.Contains(text, "Recorded outcome: VERIFIED") {
		t.Fatalf("missing post-test snapshot was treated as unchanged: %#v", out)
	}
}

func TestNewerDeliveryResultWinsWhenCompletionsAreOutOfOrder(t *testing.T) {
	tests := []struct {
		name      string
		newerExit int
		olderExit int
		wantFail  bool
	}{
		{name: "newer_failure", newerExit: 1, olderExit: 0, wantFail: true},
		{name: "newer_success", newerExit: 0, olderExit: 1, wantFail: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, id := range []string{"older", "newer"} {
				hook(t, dir, map[string]any{
					"session_id": "s", "turn_id": test.name, "hook_event_name": "PreToolUse",
					"tool_name": "Bash", "tool_use_id": id, "tool_input": map[string]any{"command": "ship-it"},
				})
			}
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "newer", "tool_response": map[string]any{"exit_code": test.newerExit},
			})
			hook(t, dir, map[string]any{
				"session_id": "s", "turn_id": test.name, "hook_event_name": "PostToolUse",
				"tool_name": "Bash", "tool_use_id": "older", "tool_response": map[string]any{"exit_code": test.olderExit},
			})
			state := loadTestState(t, dir)
			if got := recordedOutcome(state) == outcomeFailed; got != test.wantFail {
				t.Fatalf("delivery completion order overrode invocation order: failed=%v state=%#v", got, state)
			}
		})
	}
}

func committedTestRepo(t *testing.T, contents string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	path := filepath.Join(repo, "app.go")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", repo, "add", "app.go").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
	commit := exec.Command("git", "-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "baseline")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, output)
	}
	return repo, path
}
