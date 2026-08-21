package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsePassedDetectsFailureSignalsInNestedToolOutput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "go_fail_marker_in_nested_output_rejects",
			raw:  `{"result":{"exit_code":0,"output":{"stdout":"=== RUN TestSuite\n--- FAIL: TestSuite (0.00s)\nFAIL\n"}}}`,
			want: false,
		},
		{
			name: "pytest_fail_summary_rejects",
			raw:  `{"result":{"exit_code":0,"output":{"stdout":"collected 1 item\nFAILED tests/test_api.py::test_api_behavior - AssertionError\n=========================== short test summary info ============================\nFAILED tests/test_api.py::test_api_behavior - AssertionError\n"}}}`,
			want: false,
		},
		{
			name: "npm_err_rejects",
			raw:  `{"result":{"output":{"stderr":"npm ERR! Test failed: expect(received).toBe(expected) - 1 === 2"}}}`,
			want: false,
		},
		{
			name: "nested_nonzero_exit_code_rejects",
			raw:  `{"result":{"summary":{"attempt":{"exit_code":1}}}}`,
			want: false,
		},
		{
			name: "go_json_action_fail_rejects",
			raw:  `{"result":{"output":{"Action":"fail","Package":"example"}}}`,
			want: false,
		},
		{
			name: "go_json_line_in_stdout_rejects",
			raw:  `{"result":{"stdout":"{\"Action\":\"fail\",\"Package\":\"example\"}\n"}}`,
			want: false,
		},
		{
			name: "pytest_collection_error_rejects",
			raw:  `{"result":{"output":"ERROR collecting tests/test_api.py\n1 error in 0.12s"}}`,
			want: false,
		},
		{
			name: "jest_failed_summary_rejects",
			raw:  `{"result":{"output":"Test Suites: 1 failed, 2 passed\nTests: 1 failed, 5 passed"}}`,
			want: false,
		},
		{
			name: "ansi_go_failure_rejects",
			raw:  `{"result":{"output":"\u001b[31m--- FAIL: TestSuite\u001b[0m\nFAIL"}}`,
			want: false,
		},
		{
			name: "inline_ansi_jest_summary_rejects",
			raw:  `{"result":{"output":"\u001b[1mTest Suites: \u001b[22m\u001b[31m1 failed\u001b[0m, 2 passed"}}`,
			want: false,
		},
		{
			name: "inline_ansi_pytest_marker_rejects",
			raw:  `{"result":{"output":"FAILED\u001b[0m tests/test_api.py::test_api"}}`,
			want: false,
		},
		{
			name: "go_ok_output_passes",
			raw:  `{"result":{"exit_code":0,"output":{"stdout":"=== RUN   TestSuite\n--- PASS: TestSuite (0.00s)\nPASS\n"}}}`,
			want: true,
		},
		{
			name: "pytest_pass_summary_passes",
			raw:  `{"result":{"exit_code":0,"output":{"stdout":"collected 1 item\n.\n\n=========================== 1 passed in 0.02s ============================"}}}`,
			want: true,
		},
		{
			name: "structural_exit_code_zero_passes",
			raw:  `{"result":{"metadata":{"exit_code":0}}}`,
			want: true,
		},
		{
			name: "fixture_log_starting_fail_passes",
			raw:  `{"result":{"exit_code":0,"output":"FAIL open is an expected fixture message\nPASS"}}`,
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := testResponsePassed(json.RawMessage(test.raw)); got != test.want {
				t.Fatalf("test %q responsePassed = %v, want %v for %s", test.name, got, test.want, test.raw)
			}
		})
	}
}

func TestResponsePassedRejectsInvalidJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty", raw: "", want: false},
		{name: "malformed", raw: `{"result":"oops"`, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := testResponsePassed(json.RawMessage(test.raw)); got != test.want {
				t.Fatalf("invalid response case %q = %v, want %v", test.name, got, test.want)
			}
		})
	}
}

func TestWrappedTestFailureCannotVerifyEdit(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "wrapped", "hook_event_name": "PreToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_input": map[string]any{"patch": "change"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "wrapped", "hook_event_name": "PostToolUse",
		"tool_name": "apply_patch", "tool_use_id": "edit", "tool_response": map[string]any{"exit_code": 0},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "wrapped", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "go test ./..."},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "wrapped", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"result": map[string]any{"Action": "fail", "Package": "example"}},
	})
	out := hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "wrapped", "hook_event_name": "Stop",
		"last_assistant_message": "Done",
	})
	if strings.Contains(string(mustJSON(out)), "Goal result: SUCCESS") {
		t.Fatalf("wrapped failure verified edited revision: %#v", out)
	}
}

func TestWrappedTestFailureDoesNotResetCorrectionStreak(t *testing.T) {
	dir := t.TempDir()
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "one", "hook_event_name": "UserPromptSubmit", "prompt": "Stop, wrong target"})
	hook(t, dir, map[string]any{"session_id": "s", "turn_id": "two", "hook_event_name": "UserPromptSubmit", "prompt": "Wrong target again"})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "test", "hook_event_name": "PreToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_input": map[string]any{"command": "pytest"},
	})
	hook(t, dir, map[string]any{
		"session_id": "s", "turn_id": "test", "hook_event_name": "PostToolUse",
		"tool_name": "Bash", "tool_use_id": "test", "tool_response": map[string]any{"output": "FAILED tests/test_api.py::test_api"},
	})
	out := hook(t, dir, map[string]any{"session_id": "s", "turn_id": "three", "hook_event_name": "UserPromptSubmit", "prompt": "No, stop and recheck it"})
	if text := string(mustJSON(out)); !strings.Contains(text, "Corrections are repeating") || strings.Contains(text, "Thanks for the correction") {
		t.Fatalf("wrapped failure reset correction streak: %#v", out)
	}
}
