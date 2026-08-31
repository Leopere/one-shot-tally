package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const binaryVersion = "1.16.0"

const subagentGuidance = "Main thread owns requirements, architecture, authorization, integration, and acceptance. Use explorers for evidence; workers or implementors for scoped changes; reviewers for checks."

const sparkGuidance = "Before implementation, actively look for an exact, low-risk, independent, disjoint edit for spark_worker. When one exists, give exact files, expected behavior, and validation; otherwise continue in the main thread. Never use Spark for security judgment, infrastructure, credentials, ship/deploy, destructive/billable, sequential work, or overlapping ownership."

const communicationGuidance = "Keep non-code agent messages terse; preserve exact technical terms."

const targetGuidance = "Before external changes, state target and visible result: evidence, not permission. If clear, continue without waiting."

const acceptanceGuidance = "A standing user instruction to ship completed changes to production authorizes matching revisions through an already-trusted tracked contract until revoked. Do not ask per-revision. Only the user may authorize a new target or deployment trust. Otherwise present the procedure once; after acceptance, do not ask again."

const deliveryGuidance = "After the latest edit succeeds and is verified, run ship-it immediately. Do not recommend it, ask separate shipping permission, or wait. With no production deploy-it contract, state target, revision, and visible acceptance procedure once; do not stop at the push. " + acceptanceGuidance + " Continue through ship-it/deploy-it and verify; never invent trust. Delivery failure is not a stopping point: fix its in-scope cause, then resume the same authorized trusted handoff. Never blindly retry."

const runnerGuidance = "Never rely on GitHub Actions public or GitHub-hosted runners. Use the existing self-hosted local runner and its labels documented in ~/dev/gh-runner. If it stalls or is insufficient, diagnose and fix that runner first. Treat GitHub-hosted runner labels as a shipping defect."

var (
	// Test runners must begin a shell command segment. Matching a bare "test"
	// token anywhere lets harmless commands such as "echo test" forge a passing
	// verification for the current revision.
	testRE               = regexp.MustCompile(`(?i)(^|[;&|])\s*([a-z_][a-z0-9_]*=\S+\s+)*(pytest|go\s+test|cargo\s+test|npm\s+(run\s+)?test|pnpm\s+(run\s+)?test|yarn\s+test|bun\s+test|rspec|phpunit|gradle\w*\s+test|mvn\w*\s+test|make\s+(test|check|verify)|(verify|check)(\.sh)?|(\./|[^\s;&|]+/)(test|tests|verify|check)(\.sh)?)([;&|\s]|$)`)
	readRE               = regexp.MustCompile(`(?i)^\s*(rg|grep|find|fd|sed\s+-n|head|tail|ls|stat|cat\s|git\s+(status|diff|log|show|branch|rev-parse|worktree\s+list))`)
	externalMutationRE   = regexp.MustCompile(`(?i)(curl\b[^\n]*(--request|-X)\s*(POST|PUT|PATCH|DELETE)\b|gh\s+api\b[^\n]*(--method|-X)\s*(POST|PUT|PATCH|DELETE)\b|\b(bw|op|vault)\b[^\n]*\b(create|delete|edit|update|move)\b|docker\s+(service\s+update|stack\s+deploy)|kubectl\s+(apply|delete)|terraform\s+apply)`)
	passiveWaitRE        = regexp.MustCompile(`(?i)^\s*(sleep\b|watch\b|tail\s+-f\b|while\b.*\bsleep\b|until\b.*\bsleep\b|tmux\s+(capture-pane|list-panes|list-sessions|has-session)\b)`)
	detachedTmuxRE       = regexp.MustCompile(`(?i)\btmux\s+(new-session|new)\b[^\n]*(\s-d\b|-d\s)`)
	correctionScopeRE    = regexp.MustCompile(`(?i)\b(repo|repository|target|branch|worktree|workspace|directory|folder|environment|production|staging|deploy|deployment|file|edit|codebase|correction)\b`)
	gitCommitRE          = regexp.MustCompile(`(?i)^\s*git\s+commit\b`)
	backgroundRecordRE   = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+record\b`)
	backgroundCompleteRE = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+complete\b`)
	todoAddRE            = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+add\b`)
	todoDoneRE           = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+done\b`)
)

const (
	outcomeNoWork   = "NO OBSERVED WORK"
	outcomeActivity = "ACTIVITY OBSERVED"
	outcomeFailed   = "FAILED"
	outcomeVerified = "VERIFIED"
)

type event struct {
	SessionID        string          `json:"session_id"`
	TurnID           string          `json:"turn_id"`
	HookEventName    string          `json:"hook_event_name"`
	ToolName         string          `json:"tool_name"`
	ToolUseID        string          `json:"tool_use_id"`
	ToolInput        json.RawMessage `json:"tool_input"`
	ToolResponse     json.RawMessage `json:"tool_response"`
	StopHookActive   bool            `json:"stop_hook_active"`
	LastAssistantMsg string          `json:"last_assistant_message"`
	Prompt           string          `json:"prompt"`
	CWD              string          `json:"cwd"`
}

type pendingCall struct {
	Test               bool      `json:"test"`
	Production         bool      `json:"production"`
	Revision           int       `json:"revision"`
	StartedAt          time.Time `json:"started_at"`
	RepeatedTest       bool      `json:"repeated_test"`
	BackgroundRecord   bool      `json:"background_record"`
	BackgroundComplete bool      `json:"background_complete"`
	TodoAdd            bool      `json:"todo_add"`
	TodoDone           bool      `json:"todo_done"`
	GoalTransition     string    `json:"goal_transition,omitempty"`
	Edit               bool      `json:"edit,omitempty"`
	Shipping           bool      `json:"shipping,omitempty"`
	Deploying          bool      `json:"deploying,omitempty"`
	DeployCommitMatch  bool      `json:"deploy_commit_match,omitempty"`
	ResultMarker       string    `json:"result_marker,omitempty"`
	TestEligible       bool      `json:"test_eligible,omitempty"`
	WorktreeKnown      bool      `json:"worktree_known,omitempty"`
	WorktreeSnapshot   string    `json:"worktree_snapshot,omitempty"`
	WorkingDirectory   string    `json:"working_directory,omitempty"`
	Sequence           int       `json:"sequence,omitempty"`
}

type state struct {
	StateVersion                 int                    `json:"state_version"`
	SessionID                    string                 `json:"session_id"`
	TurnID                       string                 `json:"turn_id"`
	UpdatedAt                    time.Time              `json:"updated_at"`
	TotalCalls                   int                    `json:"total_calls"`
	CallCostUnits                int                    `json:"call_cost_units"`
	SparkCalls                   int                    `json:"spark_calls"`
	Tests                        int                    `json:"tests"`
	TestPasses                   int                    `json:"test_passes"`
	TestFailures                 int                    `json:"test_failures"`
	TotalTestMillis              int64                  `json:"total_test_millis"`
	MaxTestMillis                int64                  `json:"max_test_millis"`
	RedundantTestMillis          int64                  `json:"redundant_test_millis"`
	Revision                     int                    `json:"revision"`
	SuccessfulEdits              int                    `json:"successful_edits"`
	VerifiedRevision             int                    `json:"verified_revision"`
	InspectionStreak             int                    `json:"inspection_streak"`
	MaxInspectionStreak          int                    `json:"max_inspection_streak"`
	RepeatedWarnings             int                    `json:"repeated_warnings"`
	ProductionCompletions        int                    `json:"production_completions"`
	ProductionAttempts           int                    `json:"production_attempts"`
	ShipCompletions              int                    `json:"ship_completions"`
	DeployCompletions            int                    `json:"deploy_completions"`
	ShipAttempts                 int                    `json:"ship_attempts"`
	DeployAttempts               int                    `json:"deploy_attempts"`
	BackgroundRecords            int                    `json:"background_records"`
	BackgroundCompletions        int                    `json:"background_completions"`
	PassiveWaits                 int                    `json:"passive_waits"`
	PassiveWaitStreak            int                    `json:"passive_wait_streak"`
	TodosParked                  int                    `json:"todos_parked"`
	TodosCompleted               int                    `json:"todos_completed"`
	ToolCounts                   map[string]int         `json:"tool_counts"`
	Fingerprints                 map[string]int         `json:"fingerprints"`
	TestFingerprints             map[string]int         `json:"test_fingerprints"`
	Pending                      map[string]pendingCall `json:"pending"`
	LastTestPassed               bool                   `json:"last_test_passed"`
	LastTestResultKnown          bool                   `json:"last_test_result_known"`
	LastTestResultSequence       int                    `json:"last_test_result_sequence"`
	LastEditSucceeded            bool                   `json:"last_edit_succeeded"`
	LastEditResultKnown          bool                   `json:"last_edit_result_known"`
	LastEditResultRevision       int                    `json:"last_edit_result_revision"`
	LastShipSucceeded            bool                   `json:"last_ship_succeeded"`
	LastShipResultKnown          bool                   `json:"last_ship_result_known"`
	LastShipResultSequence       int                    `json:"last_ship_result_sequence"`
	LastShipRevision             int                    `json:"last_ship_revision"`
	LastDeploySucceeded          bool                   `json:"last_deploy_succeeded"`
	LastDeployResultKnown        bool                   `json:"last_deploy_result_known"`
	LastDeployResultSequence     int                    `json:"last_deploy_result_sequence"`
	LastDeployRevision           int                    `json:"last_deploy_revision"`
	LastDeployCommitMatch        bool                   `json:"last_deploy_commit_match"`
	LastProductionSucceeded      bool                   `json:"last_production_succeeded"`
	LastProductionResultKnown    bool                   `json:"last_production_result_known"`
	LastProductionResultSequence int                    `json:"last_production_result_sequence"`
	LastCallSucceeded            bool                   `json:"last_call_succeeded"`
	LastCallResultKnown          bool                   `json:"last_call_result_known"`
	LastCallResultSequence       int                    `json:"last_call_result_sequence"`
	RecordedInLifetime           bool                   `json:"recorded_in_lifetime"`
	GoalScoped                   bool                   `json:"goal_scoped"`
}

type sessionSteer struct {
	CorrectionStreak int  `json:"correction_streak"`
	SparkCalls       int  `json:"spark_calls"`
	SparkReviewShown bool `json:"spark_review_shown"`
}

type backgroundJob struct {
	ID          string    `json:"id"`
	Cleanup     string    `json:"cleanup"`
	TmuxTarget  string    `json:"tmux_target,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	TurnID      string    `json:"turn_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type todoItem struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Context   string    `json:"context"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
}

type lifetime struct {
	Runs                 int       `json:"runs"`
	TotalScore           int       `json:"total_score"`
	AverageScore         float64   `json:"average_score"`
	VerifiedRuns         int       `json:"verified_runs"`
	RevisionVerifiedRuns int       `json:"revision_verified_runs"`
	TotalToolCalls       int       `json:"total_tool_calls"`
	TotalTests           int       `json:"total_tests"`
	TotalTestFailures    int       `json:"total_test_failures"`
	TotalTestMillis      int64     `json:"total_test_millis"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type codexGoal struct {
	ThreadID    string `json:"thread_id"`
	GoalID      string `json:"goal_id"`
	Objective   string `json:"objective"`
	Status      string `json:"status"`
	TokenBudget *int64 `json:"token_budget"`
	TokensUsed  int64  `json:"tokens_used"`
	CreatedAtMS int64  `json:"created_at_ms"`
	UpdatedAtMS int64  `json:"updated_at_ms"`
}

type hookSpecificOutput struct {
	HookEventName      string         `json:"hookEventName"`
	AdditionalContext  string         `json:"additionalContext,omitempty"`
	PermissionDecision string         `json:"permissionDecision,omitempty"`
	UpdatedInput       map[string]any `json:"updatedInput,omitempty"`
}

type hookOutput struct {
	Decision           string              `json:"decision,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	SystemMessage      string              `json:"systemMessage,omitempty"`
	HookSpecificOutput *hookSpecificOutput `json:"hookSpecificOutput,omitempty"`
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "status", "grade":
			if err := printLatest(len(os.Args) > 2 && os.Args[2] == "--json"); err != nil {
				fatal(err)
			}
			return
		case "background":
			if err := backgroundCommand(os.Args[2:], os.Stdout); err != nil {
				fatal(err)
			}
			return
		case "todo":
			if err := todoCommand(os.Args[2:], os.Stdout); err != nil {
				fatal(err)
			}
			return
		case "goal":
			if err := goalCommand(os.Args[2:], os.Stdout); err != nil {
				fatal(err)
			}
			return
		case "credential":
			if err := credentialCommand(os.Args[2:], os.Stdin, os.Stdout); err != nil {
				fatal(err)
			}
			return
		case "version":
			printVersion(os.Stdout)
			return
		case "help", "-h", "--help":
			printHelp(os.Stdout)
			return
		default:
			fatal(fmt.Errorf("usage: one-shot-tally [status [--json]|grade [--json]|background <record|complete|list>|todo <add|list|done>|goal <list|show|resume>|credential <key-check|send|receive>|version|help]"))
		}
	}
	runHookFailOpen(os.Stdin, os.Stdout, os.Stderr)
}

func runHookFailOpen(r io.Reader, w, diagnostics io.Writer) {
	if err := runHook(r, w); err != nil {
		fmt.Fprintln(diagnostics, "one-shot-tally: bookkeeping failed; tool use continues:", err)
		_ = writeJSON(w, hookOutput{})
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "one-shot-tally %s | ColinKnapp.com\n", binaryVersion)
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "one-shot-tally - mechanical one-shot delivery hook")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  one-shot-tally                  process a hook event from stdin")
	fmt.Fprintln(w, "  one-shot-tally status [--json]  show the latest run and lifetime totals")
	fmt.Fprintln(w, "  one-shot-tally grade [--json]   alias for status")
	fmt.Fprintln(w, "  one-shot-tally background record ID --cleanup CMD [--tmux-target PANE]")
	fmt.Fprintln(w, "                                  record cleanup and the agent wake-up target")
	fmt.Fprintln(w, "  one-shot-tally background complete ID [--wake]")
	fmt.Fprintln(w, "                                  mark complete; --wake notifies the recorded tmux pane")
	fmt.Fprintln(w, "  one-shot-tally background list  list recorded background jobs and cleanup commands")
	fmt.Fprintln(w, "  one-shot-tally todo add TEXT --context WHY")
	fmt.Fprintln(w, "                                  park discovered out-of-scope work and return to the goal")
	fmt.Fprintln(w, "  one-shot-tally todo list [--all] list open deferred work, or include completed items")
	fmt.Fprintln(w, "  one-shot-tally todo done ID      complete one deferred item")
	fmt.Fprintln(w, "  one-shot-tally goal list [--all] list resumable goals, or all goals")
	fmt.Fprintln(w, "  one-shot-tally goal show ID      print a previous goal")
	fmt.Fprintln(w, "  one-shot-tally goal resume ID    print the exact create_goal handoff")
	fmt.Fprintln(w, "  one-shot-tally credential key-check")
	fmt.Fprintln(w, "                                  verify the pinned GnuPG WKD recipient key")
	fmt.Fprintln(w, "  one-shot-tally credential send --operation-id UUID --account REF")
	fmt.Fprintln(w, "                                  fetch the key, then sign, encrypt, and send stdin")
	fmt.Fprintln(w, "  one-shot-tally version          show the version")
	fmt.Fprintln(w, "  one-shot-tally help|-h|--help   show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Guidance: finish the requested outcome, verify edits, and treat the score as advisory.")
	fmt.Fprintln(w, "Use goal resume to recover an objective; Codex must create the resumed goal.")
}

func runHook(r io.Reader, w io.Writer) error {
	var e event
	if err := json.NewDecoder(r).Decode(&e); err != nil {
		if errors.Is(err, io.EOF) {
			return writeJSON(w, hookOutput{})
		}
		return writeJSON(w, hookOutput{})
	}
	switch e.HookEventName {
	case "SessionStart":
		context := "Finish the latest requested outcome; verify edits. Tally score is advisory. Stay in the current repository unless user names another target. " + targetGuidance + " " + deliveryGuidance + " " + runnerGuidance + " " + subagentGuidance + " " + sparkGuidance + " " + communicationGuidance
		goalActive, err := reconcileSessionGoal(e.SessionID)
		if err != nil {
			return err
		}
		if goalActive {
			context += " Goal active: keep its full objective and close it only after verification."
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: context,
		}})
	case "UserPromptSubmit":
		return userPromptSubmit(e, w)
	case "PreToolUse":
		return preToolUse(e, w)
	case "PostToolUse":
		return postToolUse(e, w)
	case "Stop":
		return stop(e, w)
	default:
		return writeJSON(w, hookOutput{})
	}
}

func userPromptSubmit(e event, w io.Writer) error {
	p, err := sessionSteerPath(e.SessionID)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	steer, err := loadSessionSteer(p)
	if err != nil {
		return err
	}
	if !correctionPrompt(e.Prompt) {
		return writeJSON(w, hookOutput{})
	}
	steer.CorrectionStreak++
	if err := saveSessionSteer(p, steer); err != nil {
		return err
	}
	context := correctionGuidance(steer.CorrectionStreak)
	return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
		HookEventName:     "UserPromptSubmit",
		AdditionalContext: context,
	}})
}

func correctionGuidance(streak int) string {
	switch {
	case streak <= 1:
		return "Thanks for the correction. Adjust only the conflicting part, preserve compatible work, and confirm the target before the next external action."
	case streak == 2:
		return "Understood. This is another correction, so pause briefly and recheck the exact repository, target, and visible acceptance result before the next action. Preserve work that still fits."
	default:
		return "Please stop and realign before the next action. Corrections are repeating. Reconfirm the exact repository, target, authorization, and visible acceptance result; keep only work that still fits."
	}
}

func correctionPrompt(prompt string) bool {
	prompt = strings.ToLower(strings.TrimSpace(prompt))
	if strings.HasPrefix(prompt, "no, stop ") ||
		strings.HasPrefix(prompt, "actually, switch from ") ||
		strings.HasPrefix(prompt, "actually switch from ") {
		return true
	}
	if !correctionScopeRE.MatchString(prompt) {
		return false
	}
	for _, scope := range []string{"repo", "repository", "target", "branch", "worktree", "workspace", "directory", "folder", "environment", "production", "staging", "deploy", "deployment", "file", "edit", "codebase", "correction"} {
		if strings.HasPrefix(prompt, "wrong "+scope) {
			return true
		}
	}
	return strings.HasPrefix(prompt, "stop ") ||
		strings.HasPrefix(prompt, "stop,") ||
		strings.HasPrefix(prompt, "stop:") ||
		strings.HasPrefix(prompt, "please stop ") ||
		strings.Contains(prompt, "switch from ") ||
		strings.Contains(prompt, "belongs in ") ||
		strings.Contains(prompt, "meant to") && strings.Contains(prompt, "instead") ||
		strings.HasPrefix(prompt, "use ") && (strings.Contains(prompt, " instead") || strings.Contains(prompt, ", not "))
}

func deliveryInvocations(command string) (shipping, deploying bool) {
	if strings.ContainsAny(command, ";&|\n") {
		return false, false
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
	}
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false, false
	}
	commandName := filepath.Base(fields[0])
	subcommand := ""
	if len(fields) > 1 {
		subcommand = fields[1]
	}
	switch commandName {
	case "ship-it":
		shipping = !oneOf(subcommand, "start", "install", "update", "version", "help", "skill", "-h", "--help")
	case "deploy-it":
		deploying = !oneOf(subcommand, "check", "trust", "install", "version", "help", "skill", "-h", "--help")
	}
	return shipping, deploying
}

func productionInvocation(command string, shipping, deploying bool) bool {
	if shipping || deploying {
		return true
	}
	if strings.ContainsAny(command, ";&|\n") {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
	}
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	if len(fields) < 2 {
		return false
	}
	for _, field := range fields[1:] {
		if field == "--dry-run" || strings.HasPrefix(field, "--dry-run=") {
			return false
		}
	}
	switch filepath.Base(fields[0]) {
	case "git":
		for _, field := range fields[2:] {
			if field == "-n" {
				return false
			}
		}
		return fields[1] == "push"
	case "kubectl":
		return fields[1] == "apply"
	case "terraform":
		return fields[1] == "apply"
	case "docker":
		return len(fields) > 2 && (fields[1] == "stack" && fields[2] == "deploy" || fields[1] == "service" && fields[2] == "update")
	default:
		return false
	}
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func deployContractPresent(cwd string) bool {
	if strings.TrimSpace(cwd) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(cwd, ".deploy-it.json"))
	if err != nil || info.IsDir() {
		return false
	}
	return exec.Command("git", "-C", cwd, "ls-files", "--error-unmatch", ".deploy-it.json").Run() == nil
}

func preToolUse(e event, w io.Writer) error {
	p, err := statePath(e)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s, err := loadState(p, e.SessionID, e.TurnID)
	if err != nil {
		return err
	}
	goalActive, err := sessionGoalActive(e.SessionID)
	if err != nil {
		return err
	}
	goalChange := goalTransition(e)
	if goalActive || goalChange != "" {
		s.GoalScoped = true
	}
	command := commandFrom(e.ToolInput)
	isEdit := isEditTool(e.ToolName)
	isCommand := isCommandTool(e.ToolName)
	isTest := isCommand && testRE.MatchString(command) && standaloneTestCommand(command)
	isShipping, isDeploying := deliveryInvocations(command)
	isProduction := isCommand && productionInvocation(command, isShipping, isDeploying)
	isRead := readRE.MatchString(command)
	isExternalMutation := isCommand && !isRead && externalMutationRE.MatchString(command)
	isPassiveWait := passiveWait(e, command)
	isBackgroundRecord := isCommand && backgroundRecordRE.MatchString(command)
	isBackgroundComplete := isCommand && backgroundCompleteRE.MatchString(command)
	isTodoAdd := isCommand && todoAddRE.MatchString(command)
	isTodoDone := isCommand && todoDoneRE.MatchString(command)

	s.TotalCalls++
	callUnits := 4
	sparkCall := isSparkCall(e)
	if sparkCall {
		callUnits = 1
		s.SparkCalls++
	}
	s.CallCostUnits += callUnits
	s.ToolCounts[e.ToolName]++
	fp := fingerprint(e.ToolName, e.ToolInput)
	s.Fingerprints[fp]++
	repeats := s.Fingerprints[fp]
	if isEdit {
		s.Revision++
		s.InspectionStreak = 0
	} else if isRead {
		s.InspectionStreak++
		if s.InspectionStreak > s.MaxInspectionStreak {
			s.MaxInspectionStreak = s.InspectionStreak
		}
	}
	repeatedTest := false
	if isTest {
		s.Tests++
		s.TestFingerprints[fp]++
		repeatedTest = s.TestFingerprints[fp] > 1
	}
	if isPassiveWait {
		s.PassiveWaits++
		s.PassiveWaitStreak++
	}
	if isShipping {
		s.ShipAttempts++
		s.LastShipResultSequence = s.TotalCalls
		s.LastShipResultKnown = false
		s.LastShipSucceeded = false
		s.LastShipRevision = s.Revision
	}
	deployCommitMatch := false
	if isDeploying {
		s.DeployAttempts++
		s.LastDeployResultSequence = s.TotalCalls
		s.LastDeployResultKnown = false
		s.LastDeploySucceeded = false
		s.LastDeployRevision = s.Revision
		deployCommitMatch = deployCommitMatchesHead(command, e.CWD)
		s.LastDeployCommitMatch = deployCommitMatch
	}
	if isProduction {
		s.ProductionAttempts++
		s.LastProductionResultSequence = s.TotalCalls
		s.LastProductionResultKnown = false
		s.LastProductionSucceeded = false
	}
	resultMarker := ""
	var updatedInput map[string]any
	if e.ToolUseID != "" && canonicalBashTool(e.ToolName) && (isTest || isProduction) {
		resultMarker = commandResultMarker(e)
		updatedInput = markedCommandInput(e.ToolInput, command, resultMarker)
		if updatedInput == nil {
			resultMarker = ""
		}
	}
	worktreeSnapshot, worktreeKnown := "", false
	observeCommandWorktree := isCommand && !isRead && !isPassiveWait && !isBackgroundRecord && !isBackgroundComplete && !isTodoAdd && !isTodoDone && !isShipping && !isDeploying && !gitCommitRE.MatchString(command)
	if s.Revision > 0 && (isEdit || observeCommandWorktree) {
		worktreeSnapshot, worktreeKnown = gitWorktreeSnapshot(e.CWD)
	}
	if e.ToolUseID != "" {
		s.Pending[e.ToolUseID] = pendingCall{Test: isTest, Production: isProduction, Revision: s.Revision, StartedAt: time.Now().UTC(), RepeatedTest: repeatedTest, BackgroundRecord: isBackgroundRecord, BackgroundComplete: isBackgroundComplete, TodoAdd: isTodoAdd, TodoDone: isTodoDone, GoalTransition: goalChange, Edit: isEdit, Shipping: isShipping, Deploying: isDeploying, DeployCommitMatch: deployCommitMatch, ResultMarker: resultMarker, TestEligible: isTest && currentEditReady(s) && !hasPendingEdit(s), WorktreeKnown: worktreeKnown, WorktreeSnapshot: worktreeSnapshot, WorkingDirectory: e.CWD, Sequence: s.TotalCalls}
	}
	var messages []string
	if goalChange == "start" {
		messages = append(messages, "Goal mode started. High tool-call volume is expected and does not reduce the coaching score. Keep each call tied to the objective. Park unrelated work. "+subagentGuidance+" "+sparkGuidance)
	}
	if repeats == 2 && isAgentSpawn(e.ToolName) {
		messages = append(messages, "Duplicate worker check: parallel subagents are encouraged, but an identical assignment is redundant. Reuse that worker or give the new worker a distinct bounded task.")
	}
	if isEdit && s.LastTestResultKnown && !s.LastTestPassed {
		messages = append(messages, "Failure containment: keep this edit tied to the observed failing check. Do not add a new mode, module, or sibling-repository change until the affected check passes.")
	}
	if isProduction || isExternalMutation {
		messages = append(messages, "Target check (advisory, never a gate): match the latest request to the exact repository, environment, artifact or revision, and user-visible acceptance result. If they match, execute now; do not treat command success alone as goal success.")
	}
	if isPassiveWait {
		if message := pacedSteer(s.PassiveWaitStreak, "A gentle note: passive waiting adds no evidence. Detach long work and record its cleanup and wake-up target.", "A friendly nudge: passive waiting is repeating. Please record the job once and continue another useful step.", "A clear steer: repeated passive waiting is stalling the goal. Stop polling; use the recorded wake-up path or stop cleanly."); message != "" {
			messages = append(messages, message)
		}
	}
	if isCommand && detachedTmuxRE.MatchString(command) && !isBackgroundRecord {
		messages = append(messages, "Progress check: record this detached job, its cleanup command, and its tmux pane. Completion can then wake the agent without polling.")
	}
	if repeats > 1 {
		message := pacedSteer(repeats-1,
			fmt.Sprintf("A gentle note: %s received the same input again. Reuse the result if it is still current.", e.ToolName),
			fmt.Sprintf("A friendly nudge: %s has repeated the same input 4 times. Please use the existing evidence. Next: %s", e.ToolName, nextAction(s)),
			fmt.Sprintf("A clear steer: %s keeps receiving the same input. Stop repeating it unless state changed. Next: %s", e.ToolName, nextAction(s)))
		if message != "" {
			messages = append(messages, message)
		}
	}
	if repeats == 4 {
		s.RepeatedWarnings++
	}
	if isTest && s.Tests == 6 {
		messages = append(messages, "Six test runs. Avoid rerunning passing checks; continue any verification correctness requires.")
	}
	if err := save(p, s); err != nil {
		return err
	}
	if sparkCall {
		if err := recordSessionSparkCall(e.SessionID); err != nil {
			return err
		}
	}
	if len(messages) == 0 && updatedInput == nil {
		return writeJSON(w, hookOutput{})
	}
	specific := &hookSpecificOutput{HookEventName: "PreToolUse", AdditionalContext: strings.Join(messages, " ")}
	if updatedInput != nil {
		specific.PermissionDecision = "allow"
		specific.UpdatedInput = updatedInput
	}
	return writeJSON(w, hookOutput{HookSpecificOutput: specific})
}

func pacedSteer(occurrence int, gentle, firmer, clear string) string {
	switch {
	case occurrence == 1:
		return gentle
	case occurrence == 3:
		return firmer
	case occurrence >= 6 && (occurrence-6)%6 == 0:
		return clear
	default:
		return ""
	}
}

func nextAction(s state) string {
	switch {
	case s.LastTestResultKnown && !s.LastTestPassed:
		return "use the failure output to diagnose one cause, make one complete correction, then rerun only the affected check and final contract"
	case s.Revision == 0:
		return "use the evidence already gathered to take the smallest step that advances the requested goal; edit only when evidence supports a change, otherwise answer or request the missing direction"
	case s.VerifiedRevision == s.Revision && s.LastTestPassed:
		return "the current revision is verified; run ship-it now, then report success unless a stated requirement remains unmet"
	default:
		return "finish the smallest current goal step, review it once, and run the narrow check plus required final contract"
	}
}

func postToolUse(e event, w io.Writer) error {
	p, err := statePath(e)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s, err := loadState(p, e.SessionID, e.TurnID)
	if err != nil {
		return err
	}
	pending, ok := s.Pending[e.ToolUseID]
	passed := false
	resultKnown, resultSucceeded := false, false
	if ok {
		delete(s.Pending, e.ToolUseID)
		resultKnown, resultSucceeded = explicitResponseResult(e.ToolResponse, pending.ResultMarker)
		if resultKnown && pending.Sequence >= s.LastCallResultSequence {
			s.LastCallResultKnown = true
			s.LastCallSucceeded = resultSucceeded
			s.LastCallResultSequence = pending.Sequence
		}
		passed = responsePassed(e.ToolResponse)
		if pending.Test {
			passed = resultKnown && resultSucceeded
		}
		worktreeChanged, worktreeConsistent := false, true
		if pending.WorktreeKnown {
			worktreeConsistent = false
			cwd := pending.WorkingDirectory
			if strings.TrimSpace(e.CWD) != "" {
				cwd = e.CWD
			}
			if snapshot, known := gitWorktreeSnapshot(cwd); known {
				worktreeConsistent = snapshot == pending.WorktreeSnapshot
				worktreeChanged = !worktreeConsistent
				if worktreeChanged && !pending.Edit {
					recordObservedWorktreeEdit(&s, resultKnown, resultSucceeded)
					if s.LastEditSucceeded {
						s.SuccessfulEdits++
					}
				}
			}
		}
		if pending.Edit && pending.Revision >= s.LastEditResultRevision {
			s.LastEditResultKnown = resultKnown
			s.LastEditSucceeded = resultKnown && resultSucceeded
			if pending.WorktreeKnown {
				s.LastEditResultKnown = worktreeChanged || resultKnown && !resultSucceeded
				s.LastEditSucceeded = worktreeChanged && (!resultKnown || resultSucceeded)
			}
			s.LastEditResultRevision = pending.Revision
			passed = s.LastEditResultKnown && s.LastEditSucceeded
			if passed {
				s.Fingerprints = map[string]int{}
				s.PassiveWaitStreak = 0
			}
		}
		if pending.Edit && passed {
			s.SuccessfulEdits++
		}
		if pending.Test {
			duration := testDurationMillis(e.ToolResponse, pending.StartedAt)
			s.TotalTestMillis += duration
			if duration > s.MaxTestMillis {
				s.MaxTestMillis = duration
			}
			if pending.RepeatedTest {
				s.RedundantTestMillis += duration
			}
			if resultKnown && resultSucceeded {
				s.TestPasses++
				s.Fingerprints = map[string]int{}
				s.PassiveWaitStreak = 0
			} else if resultKnown {
				s.TestFailures++
			}
			if pending.Sequence >= s.LastTestResultSequence {
				s.LastTestResultSequence = pending.Sequence
				s.LastTestResultKnown = resultKnown
				s.LastTestPassed = resultKnown && resultSucceeded
				if resultKnown && resultSucceeded && pending.TestEligible && worktreeConsistent && pending.Revision == s.Revision {
					s.VerifiedRevision = pending.Revision
				}
			}
		}
		if pending.BackgroundRecord && responsePassed(e.ToolResponse) {
			s.BackgroundRecords++
		}
		if pending.BackgroundComplete && responsePassed(e.ToolResponse) && !bytes.Contains(e.ToolResponse, []byte("already completed; no wake sent")) {
			s.BackgroundCompletions++
		}
		if pending.Production && resultKnown && resultSucceeded {
			s.ProductionCompletions++
		}
		if pending.Production && pending.Sequence >= s.LastProductionResultSequence {
			s.LastProductionResultSequence = pending.Sequence
			s.LastProductionResultKnown = resultKnown
			s.LastProductionSucceeded = resultKnown && resultSucceeded
		}
		if pending.Shipping && resultKnown && resultSucceeded {
			s.ShipCompletions++
		}
		if pending.Shipping && pending.Sequence >= s.LastShipResultSequence {
			s.LastShipResultSequence = pending.Sequence
			s.LastShipResultKnown = resultKnown
			s.LastShipSucceeded = resultKnown && resultSucceeded
			s.LastShipRevision = pending.Revision
		}
		if pending.Deploying && pending.Sequence >= s.LastDeployResultSequence {
			s.LastDeployResultSequence = pending.Sequence
			s.LastDeployResultKnown = resultKnown
			s.LastDeploySucceeded = resultKnown && resultSucceeded
			s.LastDeployRevision = pending.Revision
			s.LastDeployCommitMatch = pending.DeployCommitMatch
		}
		if pending.Deploying && resultKnown && resultSucceeded {
			s.DeployCompletions++
		}
		if pending.TodoAdd && responsePassed(e.ToolResponse) {
			s.TodosParked++
		}
		if pending.TodoDone && responsePassed(e.ToolResponse) {
			s.TodosCompleted++
		}
		if pending.GoalTransition != "" && responsePassed(e.ToolResponse) {
			if err := setSessionGoalActive(e.SessionID, pending.GoalTransition == "start"); err != nil {
				return err
			}
		}
	}
	if err := save(p, s); err != nil {
		return err
	}
	if ok && passed && (pending.Edit || pending.Test) {
		if err := resetSessionSteer(e.SessionID); err != nil {
			return err
		}
	}
	return writeJSON(w, hookOutput{})
}

func stop(e event, w io.Writer) error {
	p, err := statePath(e)
	if err != nil {
		return err
	}
	deployContract := deployContractPresent(e.CWD)
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	s, err := loadState(p, e.SessionID, e.TurnID)
	if err != nil {
		return err
	}
	goalActive, err := sessionGoalActive(e.SessionID)
	if err != nil {
		return err
	}
	line := reportLine(s)
	if goalActive {
		line += " | Goal state: ACTIVE"
	}
	stewardship := ""
	if s.BackgroundRecords > s.BackgroundCompletions {
		stewardship = " Background work remains recorded: do not poll it; its completion command must wake the originating agent, which should resume the task and use the recorded cleanup command."
	}
	closure := closingLoop(s, deployContract)
	if !e.StopHookActive && !goalActive {
		sparkReview, err := claimSparkRoutingReview(e.SessionID, s)
		if err != nil {
			return err
		}
		closure += sparkReview
	}
	message := line + ". " + outcomeAdvisory(recordedOutcome(s)) + stewardship + closure
	deliveryPending := requiredDeliveryPending(s)
	if deliveryPending && e.StopHookActive {
		message += " Stop continuation already used: required delivery remains unresolved, but this hook will not block the same Stop again."
	}
	if deliveryPending && !e.StopHookActive {
		reason := strings.TrimSpace(closure)
		if reason == "" {
			reason = "The verified current revision still requires delivery. Run ship-it now and continue through its tracked deploy-it handoff."
		}
		return writeJSON(w, hookOutput{
			Decision:      "block",
			Reason:        "Deployment is required and this turn cannot stop yet. " + reason,
			SystemMessage: message,
		})
	}
	if deliveryPending || goalActive || !finalPassed(s) {
		return writeJSON(w, hookOutput{SystemMessage: message})
	}
	if !s.RecordedInLifetime {
		if err := recordLifetime(s); err != nil {
			return err
		}
		s.RecordedInLifetime = true
		if err := save(p, s); err != nil {
			return err
		}
	}
	return writeJSON(w, hookOutput{SystemMessage: line + stewardship + closure})
}

func closingLoop(s state, deployContract bool) string {
	kind, attempted, known, succeeded, _ := latestDeliveryResult(s)
	if attempted && known && !succeeded && kind == "deploy" {
		return " Closing loop: deploy-it did not complete. This is not a stopping point. Do not blindly rerun it. Diagnose the preserved failure, fix the in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if attempted && known && !succeeded && kind == "ship" {
		return " Closing loop: ship-it did not complete cleanly, and Git may already be shipped. This is not a stopping point. Preserve the failure, inspect Git and deployment state, fix the in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Do not blindly repeat the failed command. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if attempted && !known && kind == "deploy" {
		return " Closing loop: deploy-it returned no explicit result. This is not a stopping point. Inspect deployment state, fix any in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Do not blindly repeat the unresolved command. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if attempted && !known && kind == "ship" {
		return " Closing loop: ship-it returned no explicit result, and Git may already be shipped. This is not a stopping point. Inspect repository and deployment state, fix any in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if attempted && known && !succeeded {
		return " Closing loop: the recorded delivery action did not complete. This is not a stopping point. Preserve its failure, inspect external state, fix the in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if attempted && !known {
		return " Closing loop: the recorded delivery action returned no explicit result. This is not a stopping point. Inspect external state, fix any in-scope cause, rerun affected verification, then resume the same authorized trusted handoff and verify the visible result. Stop only for missing user authorization or a genuinely blocked external prerequisite."
	}
	if s.Revision == 0 {
		return ""
	}
	if !currentEditReady(s) {
		return " Closing loop: confirm that the edit completed successfully before shipping."
	}
	if !finalPassed(s) {
		return " Closing loop: verify the current revision before shipping."
	}
	if recoveredCurrentDeployment(s) {
		return " Closing loop: shipping and deployment completed. Confirm the user-visible acceptance result."
	}
	if shippedCurrentRevision(s) {
		if deployContract {
			return " Closing loop: shipping completed. Confirm the ship-it deploy-it handoff and the user-visible acceptance result. Apply matching standing production authorization without asking for per-revision permission; do not create new deployment trust without exact user authorization."
		}
		return " Closing loop: shipping completed, but no tracked .deploy-it.json is present and production was not deployed. If production was requested, self-resolve the missing handoff: determine the exact target, shipped revision, and visible acceptance procedure from evidence, then present that procedure once. " + acceptanceGuidance + " Apply matching standing authorization, implement the tracked contract or procedure, continue through ship-it/deploy-it, and verify the visible result; do not stop at the push, invent trust, or self-authorize."
	}
	if deployContract {
		return " Closing loop: verified changes are ship-ready. Run ship-it now; do not merely recommend it, ask for separate shipping permission, or pause for acknowledgement. It will hand off to the tracked deploy-it contract only when that exact contract is already trusted. Apply matching standing production authorization without asking again, then confirm the user-visible acceptance result."
	}
	return " Closing loop: verified changes are ship-ready. Run ship-it now; do not merely recommend it, ask for separate shipping permission, or pause for acknowledgement. If production was requested and no tracked .deploy-it.json exists after shipping, determine the exact target, artifact or revision, and visible acceptance procedure from evidence, then present it once. " + acceptanceGuidance + " Apply matching standing authorization, implement the tracked contract or procedure, continue through ship-it/deploy-it, and verify the visible result."
}

func outcomeAdvisory(outcome string) string {
	switch outcome {
	case outcomeNoWork:
		return "This hook observed no tool activity and cannot assess task completion."
	case outcomeActivity:
		return "This hook observed activity but cannot infer task completion or user-visible acceptance."
	case outcomeFailed:
		return "A recorded action or check failed; use that evidence to fix the cause and continue to the requested acceptance result before claiming completion."
	default:
		return "Recorded verification passed; confirm any required user-visible acceptance."
	}
}

func isAgentSpawn(tool string) bool {
	tool = strings.ToLower(tool)
	return strings.Contains(tool, "spawn_agent") || strings.Contains(tool, "task") && strings.Contains(tool, "agent")
}

func reportLine(s state) string {
	mode := ""
	coaching := "advisory"
	if s.GoalScoped {
		mode = " | Run mode: /goal (high tool-call volume expected)"
		coaching = "advisory; tool-call volume not scored"
	}
	score := fmt.Sprintf("%d/100 (%s)", numericScore(s), coaching)
	if !hasRecordedActivity(s) {
		score = "N/A (no observed work)"
	}
	return fmt.Sprintf("Recorded outcome: %s%s | Tool calls: %d (%d Spark; %s weighted) | Test runs: %d (%d pass, %d fail, %d unknown, %s total, %s redundant) | Delivery actions: %d completed (%d shipped, %d deployed) | Background jobs: %d recorded, %d completed; passive waits: %d | Deferred work: %d parked, %d completed | Coaching signals: %s", recordedOutcome(s), mode, s.TotalCalls, s.SparkCalls, formatCallUnits(s.CallCostUnits), s.Tests, s.TestPasses, s.TestFailures, unknownTests(s), formatMillis(s.TotalTestMillis), formatMillis(s.RedundantTestMillis), s.ProductionCompletions, s.ShipCompletions, s.DeployCompletions, s.BackgroundRecords, s.BackgroundCompletions, s.PassiveWaits, s.TodosParked, s.TodosCompleted, score)
}

func numericScore(s state) int {
	if s.TestFailures > 0 && !s.LastTestPassed {
		return 0
	}
	if s.Revision > 0 && (s.Tests == 0 || s.LastTestResultKnown && (s.VerifiedRevision != s.Revision || !s.LastTestPassed)) {
		return 25
	}
	score := 100
	tests := completedTests(s)
	switch {
	case tests > 5:
		score -= 35 + (tests-6)*5
	case tests == 5:
		score -= 20
	case tests == 4:
		score -= 10
	case tests == 1 && s.Revision > 0:
		score -= 8
	}
	score -= minInt(25, s.PassiveWaits*7)
	score += minInt(10, s.BackgroundRecords*5)
	if finalPassed(s) {
		score += minInt(6, s.TodosParked*2)
		if s.ProductionCompletions > 0 {
			score += 15
		}
	}
	if s.MaxInspectionStreak >= 8 {
		score -= minInt(15, s.MaxInspectionStreak-5)
	}
	if !s.GoalScoped {
		effectiveCalls := (s.CallCostUnits + 3) / 4
		if effectiveCalls > 30 {
			score -= minInt(15, (effectiveCalls-30)/3+1)
		}
	}
	if s.TotalTestMillis > 300_000 {
		score -= minInt(15, int((s.TotalTestMillis-300_000+119_999)/120_000))
	}
	if s.MaxTestMillis > 900_000 {
		score -= minInt(10, int((s.MaxTestMillis-900_000+299_999)/300_000))
	}
	if s.RedundantTestMillis > 0 {
		score -= minInt(30, int((s.RedundantTestMillis+9_999)/10_000))
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		return 100
	}
	if finalPassed(s) && score < 50 {
		return 50
	}
	return score
}

func completedTests(s state) int { return s.TestPasses + s.TestFailures }

func unknownTests(s state) int {
	unknown := s.Tests - completedTests(s)
	if unknown < 0 {
		return 0
	}
	return unknown
}

func finalPassed(s state) bool {
	return recordedOutcome(s) == outcomeVerified
}

func recordedOutcome(s state) string {
	_, deliveryAttempted, deliveryKnown, deliverySucceeded, _ := latestDeliveryResult(s)
	if deliveryAttempted && deliveryKnown && !deliverySucceeded {
		return outcomeFailed
	}
	if s.LastEditResultKnown && !s.LastEditSucceeded {
		return outcomeFailed
	}
	if s.LastTestResultKnown && !s.LastTestPassed {
		return outcomeFailed
	}
	if s.LastCallResultKnown && !s.LastCallSucceeded {
		return outcomeFailed
	}
	if deliveryAttempted && !deliveryKnown {
		return outcomeActivity
	}
	if verifiedCurrentRevision(s) {
		return outcomeVerified
	}
	if !hasRecordedActivity(s) {
		return outcomeNoWork
	}
	return outcomeActivity
}

func verifiedCurrentRevision(s state) bool {
	return currentEditReady(s) && s.LastTestResultKnown && s.LastTestPassed && s.VerifiedRevision == s.Revision
}

func requiredDeliveryPending(s state) bool {
	if currentDeliveryComplete(s) {
		return false
	}
	_, attempted, known, succeeded, _ := latestDeliveryResult(s)
	if attempted && (!known || !succeeded) {
		return true
	}
	if verifiedCurrentRevision(s) {
		return true
	}
	return s.Revision > 0 && (s.ShipAttempts > 0 || s.DeployAttempts > 0)
}

func currentDeliveryComplete(s state) bool {
	return shippedCurrentRevision(s) || recoveredCurrentDeployment(s)
}

func shippedCurrentRevision(s state) bool {
	return s.LastShipRevision == s.Revision && s.LastShipResultKnown && s.LastShipSucceeded
}

func recoveredCurrentDeployment(s state) bool {
	return s.LastShipRevision == s.Revision && s.ShipAttempts > 0 &&
		s.LastDeployRevision == s.Revision && s.LastDeployResultKnown && s.LastDeploySucceeded && s.LastDeployCommitMatch
}

func latestDeliveryResult(s state) (kind string, attempted, known, succeeded bool, revision int) {
	sequence := -1
	consider := func(candidateKind string, attempts, candidateSequence, candidateRevision int, candidateKnown, candidateSucceeded bool) {
		if attempts == 0 || candidateSequence < sequence {
			return
		}
		kind = candidateKind
		attempted = true
		known = candidateKnown
		succeeded = candidateKnown && candidateSucceeded
		revision = candidateRevision
		sequence = candidateSequence
	}
	consider("production", s.ProductionAttempts, s.LastProductionResultSequence, s.Revision, s.LastProductionResultKnown, s.LastProductionSucceeded)
	consider("ship", s.ShipAttempts, s.LastShipResultSequence, s.LastShipRevision, s.LastShipResultKnown, s.LastShipSucceeded)
	consider("deploy", s.DeployAttempts, s.LastDeployResultSequence, s.LastDeployRevision, s.LastDeployResultKnown, s.LastDeploySucceeded)
	return kind, attempted, known, succeeded, revision
}

func deployCommitMatchesHead(command, cwd string) bool {
	if strings.TrimSpace(cwd) == "" || strings.ContainsAny(command, ";&|\n") {
		return false
	}
	fields := strings.Fields(command)
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
	}
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	if len(fields) == 0 || filepath.Base(fields[0]) != "deploy-it" {
		return false
	}
	commit := ""
	for index := 1; index < len(fields); index++ {
		if fields[index] == "--commit" && index+1 < len(fields) {
			commit = fields[index+1]
			break
		}
		if strings.HasPrefix(fields[index], "--commit=") {
			commit = strings.TrimPrefix(fields[index], "--commit=")
			break
		}
	}
	if len(commit) != 40 {
		return false
	}
	for _, character := range commit {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	head, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
	return err == nil && strings.EqualFold(strings.TrimSpace(string(head)), commit)
}

func hasRecordedActivity(s state) bool {
	return s.TotalCalls > 0 || s.Revision > 0 || s.Tests > 0 || s.ProductionAttempts > 0 || s.BackgroundRecords > 0 || s.BackgroundCompletions > 0 || s.TodosParked > 0 || s.TodosCompleted > 0
}

func currentEditReady(s state) bool {
	return s.Revision > 0 && s.LastEditResultKnown && s.LastEditSucceeded && s.LastEditResultRevision == s.Revision
}

func hasPendingEdit(s state) bool {
	for _, pending := range s.Pending {
		if pending.Edit {
			return true
		}
	}
	return false
}

func recordObservedWorktreeEdit(s *state, resultKnown, resultSucceeded bool) {
	s.Revision++
	s.LastEditResultKnown = true
	s.LastEditSucceeded = !resultKnown || resultSucceeded
	s.LastEditResultRevision = s.Revision
}

func isEditTool(tool string) bool {
	name := strings.ToLower(strings.TrimSpace(tool))
	for _, separator := range []string{"__", ".", "/", ":"} {
		if index := strings.LastIndex(name, separator); index >= 0 {
			name = name[index+len(separator):]
		}
	}
	name = strings.NewReplacer("_", "", "-", "").Replace(name)
	return name == "applypatch" || name == "edit" || name == "write"
}

func standaloneTestCommand(command string) bool {
	singleQuoted, doubleQuoted, escaped, comment := false, false, false, false
	for index := 0; index < len(command); index++ {
		character := command[index]
		if comment {
			if character == '\n' {
				return false
			}
			continue
		}
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if character == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if character == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if singleQuoted {
			continue
		}
		if character == '`' || (character == '$' && index+1 < len(command) && command[index+1] == '(') {
			return false
		}
		if doubleQuoted {
			continue
		}
		if character == '#' && (index == 0 || command[index-1] == ' ' || command[index-1] == '\t') {
			comment = true
			continue
		}
		if character == ';' || character == '&' || character == '|' || character == '\n' {
			return false
		}
	}
	return true
}

func gitWorktreeSnapshot(cwd string) (string, bool) {
	if strings.TrimSpace(cwd) == "" {
		return "", false
	}
	rootOutput, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(rootOutput))
	diffOutput, err := exec.Command("git", "-C", root, "diff", "--no-ext-diff", "--binary", "HEAD", "--").Output()
	if err != nil {
		cached, cachedErr := exec.Command("git", "-C", root, "diff", "--cached", "--no-ext-diff", "--binary", "--").Output()
		working, workingErr := exec.Command("git", "-C", root, "diff", "--no-ext-diff", "--binary", "--").Output()
		if cachedErr != nil || workingErr != nil {
			return "", false
		}
		diffOutput = append(cached, working...)
	}
	pathsOutput, err := exec.Command("git", "-C", root, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return "", false
	}
	paths := bytes.Split(pathsOutput, []byte{0})
	sort.Slice(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 })
	hash := sha256.New()
	_, _ = hash.Write(diffOutput)
	_, _ = hash.Write([]byte{0})
	for _, encodedPath := range paths {
		if len(encodedPath) == 0 {
			continue
		}
		path := string(encodedPath)
		_, _ = hash.Write(encodedPath)
		_, _ = hash.Write([]byte{0})
		absolutePath := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(absolutePath)
		if err != nil {
			return "", false
		}
		_, _ = fmt.Fprintf(hash, "%d\x00%d\x00", info.Mode(), info.Size())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(absolutePath)
			if err != nil {
				return "", false
			}
			_, _ = hash.Write([]byte(target))
		case info.Mode().IsRegular():
			file, err := os.Open(absolutePath)
			if err != nil {
				return "", false
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return "", false
			}
		}
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

func passiveWait(e event, command string) bool {
	tool := strings.ToLower(e.ToolName)
	if strings.Contains(tool, "wait_agent") {
		return false
	}
	if strings.Contains(tool, "wait") {
		return true
	}
	if strings.Contains(tool, "write_stdin") {
		var fields map[string]any
		if json.Unmarshal(e.ToolInput, &fields) == nil {
			chars, present := fields["chars"]
			return !present || strings.TrimSpace(fmt.Sprint(chars)) == ""
		}
	}
	return passiveWaitRE.MatchString(command)
}

func responsePassed(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	return !containsFailure(v)
}

func commandResultMarker(e event) string {
	sum := sha256.Sum256([]byte(e.SessionID + "\x00" + e.TurnID + "\x00" + e.ToolUseID))
	return hex.EncodeToString(sum[:12])
}

func markedCommandInput(raw json.RawMessage, command, marker string) map[string]any {
	if strings.TrimSpace(command) == "" || marker == "" {
		return nil
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) != nil || input == nil {
		return nil
	}
	if _, ok := input["command"].(string); !ok {
		return nil
	}
	input["command"] = fmt.Sprintf("(\ntrap 'one_shot_tally_exit=$?; printf \"\\n__ONE_SHOT_TALLY_RESULT_%s__:%%d\\n\" \"$one_shot_tally_exit\"; exit \"$one_shot_tally_exit\"' 0\n%s\n)", marker, command)
	return input
}

func canonicalBashTool(tool string) bool {
	return strings.EqualFold(strings.TrimSpace(tool), "Bash")
}

func markedResponseResult(output, marker string) (bool, bool) {
	if marker == "" {
		return false, false
	}
	prefix := "__ONE_SHOT_TALLY_RESULT_" + marker + "__:"
	index := strings.LastIndex(output, prefix)
	if index < 0 {
		return false, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(output[index+len(prefix):]))
	if err != nil {
		return false, false
	}
	return true, code == 0
}

func explicitResponseResult(raw json.RawMessage, markers ...string) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false, false
	}
	marker := ""
	if len(markers) > 0 {
		marker = markers[0]
	}
	known, failed := false, false
	var inspect func(map[string]any, bool)
	var inspectValue func(any, bool)
	inspectValue = func(value any, root bool) {
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				inspectValue(child, false)
			}
		case string:
			text := strings.TrimSpace(item)
			if marked, succeeded := markedResponseResult(text, marker); marked {
				known = true
				failed = failed || !succeeded
				return
			}
			if text == "" || (text[0] != '{' && text[0] != '[') {
				return
			}
			var decoded any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				inspectValue(decoded, false)
			}
		case map[string]any:
			inspect(item, root)
		}
	}
	inspect = func(envelope map[string]any, root bool) {
		for key, child := range envelope {
			switch strings.ToLower(key) {
			case "exit_code", "exitcode":
				if number, ok := child.(float64); ok {
					known = true
					failed = failed || number != 0
				}
			case "success", "ok":
				if root {
					if result, ok := child.(bool); ok && !result {
						known, failed = true, true
					}
				}
			case "is_error", "iserror":
				if root {
					if result, ok := child.(bool); ok && result {
						known, failed = true, true
					}
				}
			case "error":
				if root {
					if text, ok := child.(string); ok && strings.TrimSpace(text) != "" {
						known, failed = true, true
					}
				}
			case "result", "response", "metadata", "tool_response", "structuredcontent", "structured_content", "content", "output", "text":
				inspectValue(child, false)
			}
		}
	}
	inspectValue(value, true)
	return known, known && !failed
}

func testDurationMillis(raw json.RawMessage, started time.Time) int64 {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		if seconds, ok := findNumber(value, "wall_time_seconds", "wallTimeSeconds", "duration_seconds", "durationSeconds"); ok && seconds >= 0 {
			return int64(seconds * 1000)
		}
	}
	if started.IsZero() {
		return 0
	}
	duration := time.Since(started).Milliseconds()
	if duration < 0 {
		return 0
	}
	return duration
}

func findNumber(value any, keys ...string) (float64, bool) {
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	var walk func(any) (float64, bool)
	walk = func(current any) (float64, bool) {
		switch item := current.(type) {
		case map[string]any:
			for key, child := range item {
				if wanted[key] {
					if number, ok := child.(float64); ok {
						return number, true
					}
				}
				if number, ok := walk(child); ok {
					return number, true
				}
			}
		case []any:
			for _, child := range item {
				if number, ok := walk(child); ok {
					return number, true
				}
			}
		}
		return 0, false
	}
	return walk(value)
}

func formatMillis(milliseconds int64) string {
	if milliseconds < 1000 {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return (time.Duration(milliseconds) * time.Millisecond).Round(time.Second).String()
}

func containsFailure(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			switch strings.ToLower(k) {
			case "exit_code", "exitcode", "status_code", "statuscode":
				if n, ok := value.(float64); ok && n != 0 {
					return true
				}
			case "success", "ok":
				if b, ok := value.(bool); ok && !b {
					return true
				}
			case "error", "stderr":
				if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
					return true
				}
			}
			if containsFailure(value) {
				return true
			}
		}
	case []any:
		for _, value := range x {
			if containsFailure(value) {
				return true
			}
		}
	}
	return false
}

func loadState(path string, sessionID, turnID string) (state, error) {
	s := emptyState(sessionID, turnID)
	b, err := os.ReadFile(path)
	if err == nil {
		recovered, decodeErr := decodeState(b, &s)
		if recovered || decodeErr != nil {
			if quarantineErr := quarantineState(path); quarantineErr != nil {
				return state{}, quarantineErr
			}
		}
		if decodeErr != nil {
			s = emptyState(sessionID, turnID)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return state{}, err
	}
	normalizeState(&s)
	return s, nil
}

func normalizeState(s *state) {
	if s.ToolCounts == nil {
		s.ToolCounts = map[string]int{}
	}
	if s.Fingerprints == nil {
		s.Fingerprints = map[string]int{}
	}
	if s.TestFingerprints == nil {
		s.TestFingerprints = map[string]int{}
	}
	if s.Pending == nil {
		s.Pending = map[string]pendingCall{}
	}
	if s.StateVersion < 2 {
		for id, pending := range s.Pending {
			if pending.Test && s.Tests > s.TestPasses+s.TestFailures {
				s.Tests--
			}
			delete(s.Pending, id)
		}
		s.StateVersion = 2
	}
	if s.StateVersion < 3 {
		s.StateVersion = 3
	}
	if s.StateVersion < 4 {
		s.StateVersion = 4
	}
	if s.CallCostUnits == 0 && s.TotalCalls > 0 {
		s.CallCostUnits = s.TotalCalls * 4
	}
}

func emptyState(sessionID, turnID string) state {
	return state{StateVersion: 4, SessionID: sessionID, TurnID: turnID, ToolCounts: map[string]int{}, Fingerprints: map[string]int{}, TestFingerprints: map[string]int{}, Pending: map[string]pendingCall{}}
}

func decodeState(b []byte, s *state) (bool, error) {
	var decoded state
	if err := json.Unmarshal(b, &decoded); err == nil {
		*s = decoded
		return false, nil
	}
	decoded = state{}
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&decoded); err != nil {
		return false, err
	}
	*s = decoded
	return true, nil
}

func quarantineState(path string) error {
	backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UTC().UnixNano())
	return os.Rename(path, backup)
}

func save(path string, s state) error {
	s.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func acquireStateLock(path string) (func() error, error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			token := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
			if _, err := f.WriteString(token); err != nil {
				_ = f.Close()
				_ = os.Remove(lockPath)
				return nil, err
			}
			if err := f.Close(); err != nil {
				_ = os.Remove(lockPath)
				return nil, err
			}
			return func() error { return removeOwnedLock(lockPath, token) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, statErr := os.Stat(lockPath)
		if statErr == nil && time.Since(info.ModTime()) > time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for state lock: %s", filepath.Base(path))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func removeOwnedLock(path, token string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(b) != token {
		return nil
	}
	return os.Remove(path)
}

func stateDir() (string, error) {
	dir := os.Getenv("ONE_SHOT_STATE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".codex", "state", "one-shot-delivery")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func statePath(e event) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(e.SessionID + ":" + e.TurnID))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".json"), nil
}

func sessionSteerPath(sessionID string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(dir, "steer-"+hex.EncodeToString(sum[:12])+".state"), nil
}

func loadSessionSteer(path string) (sessionSteer, error) {
	var steer sessionSteer
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return steer, nil
	}
	if err != nil {
		return steer, err
	}
	if err := json.Unmarshal(b, &steer); err != nil {
		if quarantineErr := quarantineState(path); quarantineErr != nil {
			return sessionSteer{}, quarantineErr
		}
		return sessionSteer{}, nil
	}
	return steer, nil
}

func saveSessionSteer(path string, steer sessionSteer) error {
	b, err := json.Marshal(steer)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".steer-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func resetSessionSteer(sessionID string) error {
	p, err := sessionSteerPath(sessionID)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	steer, err := loadSessionSteer(p)
	if err != nil || steer.CorrectionStreak == 0 {
		return err
	}
	steer.CorrectionStreak = 0
	return saveSessionSteer(p, steer)
}

func recordSessionSparkCall(sessionID string) error {
	p, err := sessionSteerPath(sessionID)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	steer, err := loadSessionSteer(p)
	if err != nil {
		return err
	}
	steer.SparkCalls++
	return saveSessionSteer(p, steer)
}

func claimSparkRoutingReview(sessionID string, s state) (string, error) {
	if !finalPassed(s) || s.SuccessfulEdits < 2 {
		return "", nil
	}
	p, err := sessionSteerPath(sessionID)
	if err != nil {
		return "", err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = unlock() }()
	steer, err := loadSessionSteer(p)
	if err != nil {
		return "", err
	}
	if steer.SparkCalls > 0 || steer.SparkReviewShown {
		return "", nil
	}
	steer.SparkReviewShown = true
	if err := saveSessionSteer(p, steer); err != nil {
		return "", err
	}
	return " Spark review: none used this run. Before the next implementation, check whether an exact, low-risk, independent edit with disjoint ownership exists for spark_worker; do not invent one.", nil
}

func goalMarkerPath(sessionID string) (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(sessionID))
	return filepath.Join(dir, "goal-"+hex.EncodeToString(sum[:12])+".active"), nil
}

func sessionGoalActive(sessionID string) (bool, error) {
	p, err := goalMarkerPath(sessionID)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func reconcileSessionGoal(sessionID string) (bool, error) {
	status, found, err := codexGoalStatus(sessionID)
	if err != nil || !found {
		return sessionGoalActive(sessionID)
	}
	active := status == "active"
	if err := setSessionGoalActive(sessionID, active); err != nil {
		return false, err
	}
	return active, nil
}

func setSessionGoalActive(sessionID string, active bool) error {
	p, err := goalMarkerPath(sessionID)
	if err != nil {
		return err
	}
	if active {
		return os.WriteFile(p, []byte("active\n"), 0o600)
	}
	err = os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func goalTransition(e event) string {
	tool := strings.ToLower(strings.TrimSpace(e.ToolName))
	if strings.HasSuffix(tool, "create_goal") {
		return "start"
	}
	if !strings.HasSuffix(tool, "update_goal") {
		return ""
	}
	var input struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(e.ToolInput, &input) != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(input.Status)) {
	case "complete", "blocked":
		return "finish"
	default:
		return ""
	}
}

func isCommandTool(tool string) bool {
	tool = strings.ToLower(tool)
	return tool == "bash" || strings.Contains(tool, "exec_command") || strings.Contains(tool, "shell") || strings.Contains(tool, "terminal")
}

func goalCommand(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: one-shot-tally goal <list|show|resume>")
	}
	switch args[0] {
	case "list":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--all") {
			return errors.New("usage: one-shot-tally goal list [--all]")
		}
		return listCodexGoals(len(args) == 2, w)
	case "show", "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: one-shot-tally goal %s ID", args[0])
		}
		goal, err := findCodexGoal(args[1])
		if err != nil {
			return err
		}
		if args[0] == "show" {
			printCodexGoal(goal, w)
			return nil
		}
		fmt.Fprintf(w, "Resume saved goal %s (previous status: %s).\n\n%s\n\nCall get_goal first; do not replace an unfinished current goal. Then call create_goal with that exact objective", goal.GoalID, goal.Status, goal.Objective)
		if goal.TokenBudget != nil {
			fmt.Fprintf(w, " and token_budget %d", *goal.TokenBudget)
		}
		fmt.Fprintln(w, ". This command does not change Codex goal state.")
		return nil
	default:
		return fmt.Errorf("unknown goal command %q", args[0])
	}
}

func listCodexGoals(includeComplete bool, w io.Writer) error {
	where := "WHERE status <> 'complete'"
	if includeComplete {
		where = ""
	}
	goals, err := queryCodexGoals(fmt.Sprintf(`SELECT thread_id, goal_id, objective, status, token_budget, tokens_used, created_at_ms, updated_at_ms FROM thread_goals %s ORDER BY updated_at_ms DESC`, where))
	if err != nil {
		return err
	}
	for _, goal := range goals {
		objective := truncateRunes(singleLine(goal.Objective), 100)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", shortGoalID(goal.GoalID), goal.Status, formatGoalTime(goal.UpdatedAtMS), objective)
	}
	return nil
}

func findCodexGoal(id string) (codexGoal, error) {
	if !regexp.MustCompile(`^[A-Fa-f0-9-]{4,36}$`).MatchString(id) {
		return codexGoal{}, errors.New("goal ID must be a UUID or a prefix of at least four characters")
	}
	goals, err := queryCodexGoals(fmt.Sprintf(`SELECT thread_id, goal_id, objective, status, token_budget, tokens_used, created_at_ms, updated_at_ms FROM thread_goals WHERE goal_id LIKE '%s%%' ORDER BY updated_at_ms DESC LIMIT 2`, id))
	if err != nil {
		return codexGoal{}, err
	}
	if len(goals) == 0 {
		return codexGoal{}, fmt.Errorf("goal %q was not found", id)
	}
	if len(goals) > 1 {
		return codexGoal{}, fmt.Errorf("goal prefix %q is ambiguous", id)
	}
	return goals[0], nil
}

func printCodexGoal(goal codexGoal, w io.Writer) {
	fmt.Fprintf(w, "ID: %s\nStatus: %s\nCreated: %s\nUpdated: %s\nTokens used: %d\nObjective:\n%s\n", goal.GoalID, goal.Status, formatGoalTime(goal.CreatedAtMS), formatGoalTime(goal.UpdatedAtMS), goal.TokensUsed, goal.Objective)
}

func shortGoalID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

func formatGoalTime(milliseconds int64) string {
	return time.UnixMilli(milliseconds).Local().Format("2006-01-02 15:04")
}

func codexGoalStatus(sessionID string) (string, bool, error) {
	if sessionID == "" || !regexp.MustCompile(`^[A-Za-z0-9-]+$`).MatchString(sessionID) {
		return "", false, nil
	}
	goals, err := queryCodexGoals(fmt.Sprintf(`SELECT thread_id, goal_id, objective, status, token_budget, tokens_used, created_at_ms, updated_at_ms FROM thread_goals WHERE thread_id = '%s' LIMIT 1`, sessionID))
	if err != nil {
		return "", false, err
	}
	if len(goals) == 0 {
		return "", false, nil
	}
	return goals[0].Status, true, nil
}

func queryCodexGoals(query string) ([]codexGoal, error) {
	path, err := codexGoalsPath()
	if err != nil {
		return nil, err
	}
	output, err := exec.Command("sqlite3", "-readonly", "-json", path, query).Output()
	if err != nil {
		return nil, fmt.Errorf("read Codex goals: %w", err)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return nil, nil
	}
	var goals []codexGoal
	if err := json.Unmarshal(output, &goals); err != nil {
		return nil, fmt.Errorf("decode Codex goals: %w", err)
	}
	return goals, nil
}

func codexGoalsPath() (string, error) {
	if path := os.Getenv("ONE_SHOT_GOALS_DB"); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("Codex goals database is unavailable: %w", err)
		}
		return path, nil
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		codexHome = filepath.Join(home, ".codex")
	}
	path := filepath.Join(codexHome, "goals_1.sqlite")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("Codex goals database is unavailable: %w", err)
	}
	return path, nil
}

func isSparkCall(e event) bool {
	if strings.Contains(strings.ToLower(e.ToolName), "spark") {
		return true
	}
	var fields map[string]any
	if json.Unmarshal(e.ToolInput, &fields) != nil {
		return false
	}
	for _, key := range []string{"agent_type", "model", "subagent_type"} {
		if value, ok := fields[key].(string); ok && strings.Contains(strings.ToLower(value), "spark") {
			return true
		}
	}
	return false
}

func formatCallUnits(units int) string {
	whole := units / 4
	fraction := units % 4
	if fraction == 0 {
		return fmt.Sprintf("%d", whole)
	}
	return fmt.Sprintf("%d.%02d", whole, fraction*25)
}

func commandFrom(raw json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd"} {
		if value, ok := fields[key].(string); ok {
			return value
		}
	}
	return ""
}

func backgroundCommand(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: one-shot-tally background <record|complete|list>")
	}
	switch args[0] {
	case "record":
		return recordBackground(args[1:], w)
	case "complete":
		return completeBackground(args[1:], w)
	case "list":
		jobs, err := loadBackgroundJobs()
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(jobs))
		for id := range jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			job := jobs[id]
			status := "running"
			if !job.CompletedAt.IsZero() {
				status = "completed"
			}
			fmt.Fprintf(w, "%s\t%s\tcleanup: %s\twake: %s\n", job.ID, status, job.Cleanup, valueOr(job.TmuxTarget, "none"))
		}
		return nil
	default:
		return fmt.Errorf("unknown background command %q", args[0])
	}
}

func recordBackground(args []string, w io.Writer) error {
	if len(args) < 3 || args[1] != "--cleanup" || strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[2]) == "" {
		return errors.New("usage: one-shot-tally background record ID --cleanup CMD [--tmux-target PANE]")
	}
	id := args[0]
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(id) {
		return errors.New("background job ID may contain only letters, digits, dot, underscore, and hyphen")
	}
	target := os.Getenv("TMUX_PANE")
	if len(args) > 3 {
		if len(args) != 5 || args[3] != "--tmux-target" || strings.TrimSpace(args[4]) == "" {
			return errors.New("usage: one-shot-tally background record ID --cleanup CMD [--tmux-target PANE]")
		}
		target = args[4]
	}
	p, err := backgroundJobsPath()
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	jobs, err := loadBackgroundJobs()
	if err != nil {
		return err
	}
	jobs[id] = backgroundJob{ID: id, Cleanup: args[2], TmuxTarget: target, SessionID: os.Getenv("ONE_SHOT_SESSION_ID"), TurnID: os.Getenv("ONE_SHOT_TURN_ID"), CreatedAt: time.Now().UTC()}
	if err := saveBackgroundJobs(jobs); err != nil {
		return err
	}
	fmt.Fprintf(w, "Recorded background job %s. Arrange detached completion with: one-shot-tally background complete %s --wake\n", id, id)
	return nil
}

func completeBackground(args []string, w io.Writer) error {
	if len(args) < 1 || len(args) > 2 || (len(args) == 2 && args[1] != "--wake") {
		return errors.New("usage: one-shot-tally background complete ID [--wake]")
	}
	wake := len(args) == 2
	p, err := backgroundJobsPath()
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer func() { _ = unlock() }()
	jobs, err := loadBackgroundJobs()
	if err != nil {
		return err
	}
	job, ok := jobs[args[0]]
	if !ok {
		return fmt.Errorf("background job %q is not recorded", args[0])
	}
	if !job.CompletedAt.IsZero() {
		fmt.Fprintf(w, "Background job %s was already completed; no wake sent.\n", job.ID)
		return nil
	}
	job.CompletedAt = time.Now().UTC()
	jobs[job.ID] = job
	if err := saveBackgroundJobs(jobs); err != nil {
		return err
	}
	if wake && job.TmuxTarget != "" {
		message := fmt.Sprintf("Background job %s completed. Inspect its result only if it belongs to the active task.", job.ID)
		if err := exec.Command("tmux", "send-keys", "-l", "-t", job.TmuxTarget, message).Run(); err != nil {
			return fmt.Errorf("job recorded complete, but its one wake attempt failed and will not be retried automatically: %w", err)
		}
		if err := exec.Command("tmux", "send-keys", "-t", job.TmuxTarget, "Enter").Run(); err != nil {
			return fmt.Errorf("job recorded complete, but its one Enter attempt failed and will not be retried automatically: %w", err)
		}
	}
	fmt.Fprintf(w, "Completed background job %s; cleanup: %s\n", job.ID, job.Cleanup)
	return nil
}

func loadBackgroundJobs() (map[string]backgroundJob, error) {
	p, err := backgroundJobsPath()
	if err != nil {
		return nil, err
	}
	jobs := map[string]backgroundJob{}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return jobs, nil
	}
	if err != nil {
		return nil, err
	}
	return jobs, json.Unmarshal(b, &jobs)
}

func saveBackgroundJobs(jobs map[string]backgroundJob) error {
	p, err := backgroundJobsPath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(jobs)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func backgroundJobsPath() (string, error) {
	p, err := lifetimePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "background-jobs.json"), nil
}

func todoCommand(args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: one-shot-tally todo <add|list|done>")
	}
	switch args[0] {
	case "add":
		if len(args) != 4 || args[2] != "--context" {
			return errors.New("usage: one-shot-tally todo add TEXT --context WHY")
		}
		return addTodo(args[1], args[3], w)
	case "list":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--all") {
			return errors.New("usage: one-shot-tally todo list [--all]")
		}
		return listTodos(len(args) == 2, w)
	case "done":
		if len(args) != 2 {
			return errors.New("usage: one-shot-tally todo done ID")
		}
		return completeTodo(args[1], w)
	default:
		return fmt.Errorf("unknown todo command %q", args[0])
	}
}

func addTodo(text, context string, w io.Writer) error {
	text, context = singleLine(text), singleLine(context)
	if text == "" || context == "" {
		return errors.New("TODO text and deferral context are required")
	}
	source, err := os.Getwd()
	if err != nil {
		return err
	}
	idSum := sha256.Sum256([]byte(strings.ToLower(source + "\n" + text)))
	id := hex.EncodeToString(idSum[:5])
	items, err := loadTodos()
	if err != nil {
		return err
	}
	if _, exists := items[id]; exists {
		return fmt.Errorf("TODO %s already exists; do not duplicate deferred work", id)
	}
	items[id] = todoItem{ID: id, Text: text, Context: context, Source: source, CreatedAt: time.Now().UTC()}
	if err := saveTodos(items); err != nil {
		return err
	}
	fmt.Fprintf(w, "Parked TODO %s: %s. Return to the current goal.\n", id, text)
	return nil
}

func listTodos(includeDone bool, w io.Writer) error {
	items, err := loadTodos()
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(items))
	for id, item := range items {
		if includeDone || item.DoneAt.IsZero() {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := items[ids[i]], items[ids[j]]
		if left.CreatedAt.Equal(right.CreatedAt) {
			return left.ID < right.ID
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
	for _, id := range ids {
		item := items[id]
		status := "open"
		if !item.DoneAt.IsZero() {
			status = "done"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tcontext: %s\tsource: %s\n", item.ID, status, item.Text, item.Context, item.Source)
	}
	return nil
}

func completeTodo(id string, w io.Writer) error {
	items, err := loadTodos()
	if err != nil {
		return err
	}
	item, exists := items[id]
	if !exists {
		return fmt.Errorf("TODO %q is not recorded", id)
	}
	if !item.DoneAt.IsZero() {
		return fmt.Errorf("TODO %s is already complete", id)
	}
	item.DoneAt = time.Now().UTC()
	items[id] = item
	if err := saveTodos(items); err != nil {
		return err
	}
	fmt.Fprintf(w, "Completed TODO %s: %s\n", id, item.Text)
	return nil
}

func loadTodos() (map[string]todoItem, error) {
	p, err := todosPath()
	if err != nil {
		return nil, err
	}
	items := map[string]todoItem{}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	return items, json.Unmarshal(b, &items)
}

func saveTodos(items map[string]todoItem) error {
	p, err := todosPath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(items)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func todosPath() (string, error) {
	p, err := lifetimePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "todos.json"), nil
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func fingerprint(tool string, raw json.RawMessage) string {
	var value any
	_ = json.Unmarshal(raw, &value)
	normalized, _ := json.Marshal(value)
	sum := sha256.Sum256(append([]byte(tool+":"), normalized...))
	return hex.EncodeToString(sum[:10])
}

func printLatest(asJSON bool) error {
	return printLatestTo(os.Stdout, asJSON)
}

func printLatestTo(w io.Writer, asJSON bool) error {
	dir := os.Getenv("ONE_SHOT_STATE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".codex", "state", "one-shot-delivery")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var states []state
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "lifetime.json" || entry.Name() == "background-jobs.json" || entry.Name() == "todos.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var candidate state
		if _, err := decodeState(b, &candidate); err != nil {
			continue
		}
		normalizeState(&candidate)
		if candidate.UpdatedAt.IsZero() {
			if info, err := entry.Info(); err == nil {
				candidate.UpdatedAt = info.ModTime()
			}
		}
		states = append(states, candidate)
	}
	if len(states) == 0 {
		return errors.New("no tally state found")
	}
	s := selectStatusState(states)
	life, _ := loadLifetime()
	if asJSON {
		var coachingScore any
		if hasRecordedActivity(s) {
			coachingScore = numericScore(s)
		}
		return writeJSON(w, map[string]any{"state": s, "outcome": recordedOutcome(s), "verified": finalPassed(s), "coaching_score": coachingScore, "report": reportLine(s), "lifetime": life})
	}
	fmt.Fprintln(w, reportLine(s))
	if life.Runs > 0 {
		fmt.Fprintf(w, "Lifetime: Verified revisions since 1.12: %d | Historical verified/successful runs: %d/%d | Coaching average: %.1f/100 | Tests: %d (%d failed, %s total) | Tool calls: %d\n", life.RevisionVerifiedRuns, life.VerifiedRuns, life.Runs, life.AverageScore, life.TotalTests, life.TotalTestFailures, formatMillis(life.TotalTestMillis), life.TotalToolCalls)
	}
	return nil
}

func selectStatusState(states []state) state {
	if len(states) == 0 {
		return state{}
	}
	type sessionPick struct {
		latest     state
		latestWork state
		hasWork    bool
	}
	bySession := map[string]*sessionPick{}
	order := make([]string, 0, len(states))
	for _, candidate := range states {
		key := candidate.SessionID
		if key == "" {
			key = candidate.TurnID
		}
		pick, ok := bySession[key]
		if !ok {
			pick = &sessionPick{latest: candidate}
			bySession[key] = pick
			order = append(order, key)
		}
		if candidate.UpdatedAt.After(pick.latest.UpdatedAt) {
			pick.latest = candidate
		}
		if hasRecordedActivity(candidate) && (!pick.hasWork || candidate.UpdatedAt.After(pick.latestWork.UpdatedAt)) {
			pick.latestWork = candidate
			pick.hasWork = true
		}
	}
	var best *sessionPick
	for _, key := range order {
		pick := bySession[key]
		if best == nil || (pick.hasWork && !best.hasWork) {
			best = pick
			continue
		}
		if pick.hasWork != best.hasWork {
			continue
		}
		candidateTime, bestTime := pick.latest.UpdatedAt, best.latest.UpdatedAt
		if pick.hasWork {
			candidateTime, bestTime = pick.latestWork.UpdatedAt, best.latestWork.UpdatedAt
		}
		if candidateTime.After(bestTime) {
			best = pick
		}
	}
	if best.hasWork {
		return best.latestWork
	}
	return best.latest
}

func recordLifetime(s state) error {
	life, err := loadLifetime()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	life.Runs++
	life.TotalScore += numericScore(s)
	life.AverageScore = float64(life.TotalScore) / float64(life.Runs)
	if finalPassed(s) {
		life.VerifiedRuns++
		life.RevisionVerifiedRuns++
	}
	life.TotalToolCalls += s.TotalCalls
	life.TotalTests += completedTests(s)
	life.TotalTestFailures += s.TestFailures
	life.TotalTestMillis += s.TotalTestMillis
	life.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(life)
	if err != nil {
		return err
	}
	p, err := lifetimePath()
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func loadLifetime() (lifetime, error) {
	p, err := lifetimePath()
	if err != nil {
		return lifetime{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return lifetime{}, err
	}
	var life lifetime
	if err := json.Unmarshal(b, &life); err != nil {
		return lifetime{}, err
	}
	return life, nil
}

func lifetimePath() (string, error) {
	dir := os.Getenv("ONE_SHOT_STATE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".codex", "state", "one-shot-delivery")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "lifetime.json"), nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeJSON(w io.Writer, value any) error { return json.NewEncoder(w).Encode(value) }
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "one-shot-tally:", err)
	code := 1
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		code = exitErr.ExitCode()
	}
	os.Exit(code)
}
