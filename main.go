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
	"strings"
	"time"
)

const binaryVersion = "1.11.1"

const subagentGuidance = "The main thread owns requirements, architecture, authorization, integration, and acceptance. Use explorers for evidence, workers or implementors for scoped changes, and reviewers for checks."

const sparkGuidance = "Before implementation, actively look for an exact, low-risk, independent edit with a disjoint target for spark_worker. When one exists, give exact files, expected behavior, and validation; otherwise continue in the main thread. Never use Spark for security judgment, infrastructure, credentials, ship/deploy, destructive or billable work, sequential work, or overlapping ownership."

const communicationGuidance = "Keep non-code agent messages terse and preserve exact technical terms."

var (
	// Test runners must begin a shell command segment. Matching a bare "test"
	// token anywhere lets harmless commands such as "echo test" forge a passing
	// verification for the current revision.
	testRE               = regexp.MustCompile(`(?i)(^|[;&|])\s*([a-z_][a-z0-9_]*=\S+\s+)*(pytest|go\s+test|cargo\s+test|npm\s+(run\s+)?test|pnpm\s+(run\s+)?test|yarn\s+test|bun\s+test|rspec|phpunit|gradle\w*\s+test|mvn\w*\s+test|make\s+(test|check|verify)|(verify|check)(\.sh)?|(\./|[^\s;&|]+/)(test|tests|verify|check)(\.sh)?)([;&|\s]|$)`)
	readRE               = regexp.MustCompile(`(?i)^\s*(rg|grep|find|fd|sed\s+-n|head|tail|ls|stat|cat\s|git\s+(status|diff|log|show|branch|rev-parse|worktree\s+list))`)
	productionRE         = regexp.MustCompile("(?i)(git\\s+push|ship-it\\s*$|(depl" + "oy|rele" + "ase)[^;&|]*(prod|production)|prod\\s+depl" + "oy|kubectl\\s+apply|docker\\s+stack\\s+depl" + "oy|terraform\\s+apply)")
	externalMutationRE   = regexp.MustCompile(`(?i)(curl\b[^\n]*(--request|-X)\s*(POST|PUT|PATCH|DELETE)\b|gh\s+api\b[^\n]*(--method|-X)\s*(POST|PUT|PATCH|DELETE)\b|\b(bw|op|vault)\b[^\n]*\b(create|delete|edit|update|move)\b|docker\s+(service\s+update|stack\s+deploy)|kubectl\s+(apply|delete)|terraform\s+apply)`)
	passiveWaitRE        = regexp.MustCompile(`(?i)^\s*(sleep\b|watch\b|tail\s+-f\b|while\b.*\bsleep\b|until\b.*\bsleep\b|tmux\s+(capture-pane|list-panes|list-sessions|has-session)\b)`)
	detachedTmuxRE       = regexp.MustCompile(`(?i)\btmux\s+(new-session|new)\b[^\n]*(\s-d\b|-d\s)`)
	backgroundRecordRE   = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+record\b`)
	backgroundCompleteRE = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+complete\b`)
	todoAddRE            = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+add\b`)
	todoDoneRE           = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+done\b`)
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
}

type state struct {
	StateVersion           int                    `json:"state_version"`
	SessionID              string                 `json:"session_id"`
	TurnID                 string                 `json:"turn_id"`
	UpdatedAt              time.Time              `json:"updated_at"`
	TotalCalls             int                    `json:"total_calls"`
	CallCostUnits          int                    `json:"call_cost_units"`
	SparkCalls             int                    `json:"spark_calls"`
	Tests                  int                    `json:"tests"`
	TestPasses             int                    `json:"test_passes"`
	TestFailures           int                    `json:"test_failures"`
	TotalTestMillis        int64                  `json:"total_test_millis"`
	MaxTestMillis          int64                  `json:"max_test_millis"`
	RedundantTestMillis    int64                  `json:"redundant_test_millis"`
	Revision               int                    `json:"revision"`
	SuccessfulEdits        int                    `json:"successful_edits"`
	VerifiedRevision       int                    `json:"verified_revision"`
	InspectionStreak       int                    `json:"inspection_streak"`
	MaxInspectionStreak    int                    `json:"max_inspection_streak"`
	RepeatedWarnings       int                    `json:"repeated_warnings"`
	ProductionCompletions  int                    `json:"production_completions"`
	ShipCompletions        int                    `json:"ship_completions"`
	DeployCompletions      int                    `json:"deploy_completions"`
	ShipAttempts           int                    `json:"ship_attempts"`
	DeployAttempts         int                    `json:"deploy_attempts"`
	BackgroundRecords      int                    `json:"background_records"`
	BackgroundCompletions  int                    `json:"background_completions"`
	PassiveWaits           int                    `json:"passive_waits"`
	PassiveWaitStreak      int                    `json:"passive_wait_streak"`
	TodosParked            int                    `json:"todos_parked"`
	TodosCompleted         int                    `json:"todos_completed"`
	ToolCounts             map[string]int         `json:"tool_counts"`
	Fingerprints           map[string]int         `json:"fingerprints"`
	TestFingerprints       map[string]int         `json:"test_fingerprints"`
	Pending                map[string]pendingCall `json:"pending"`
	LastTestPassed         bool                   `json:"last_test_passed"`
	LastTestResultKnown    bool                   `json:"last_test_result_known"`
	LastEditSucceeded      bool                   `json:"last_edit_succeeded"`
	LastEditResultKnown    bool                   `json:"last_edit_result_known"`
	LastEditResultRevision int                    `json:"last_edit_result_revision"`
	LastShipSucceeded      bool                   `json:"last_ship_succeeded"`
	LastShipResultKnown    bool                   `json:"last_ship_result_known"`
	LastDeploySucceeded    bool                   `json:"last_deploy_succeeded"`
	LastDeployResultKnown  bool                   `json:"last_deploy_result_known"`
	RecordedInLifetime     bool                   `json:"recorded_in_lifetime"`
	GoalScoped             bool                   `json:"goal_scoped"`
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
	Runs              int       `json:"runs"`
	TotalScore        int       `json:"total_score"`
	AverageScore      float64   `json:"average_score"`
	VerifiedRuns      int       `json:"verified_runs"`
	TotalToolCalls    int       `json:"total_tool_calls"`
	TotalTests        int       `json:"total_tests"`
	TotalTestFailures int       `json:"total_test_failures"`
	TotalTestMillis   int64     `json:"total_test_millis"`
	UpdatedAt         time.Time `json:"updated_at"`
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
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
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
		case "version":
			printVersion(os.Stdout)
			return
		case "help", "-h", "--help":
			printHelp(os.Stdout)
			return
		default:
			fatal(fmt.Errorf("usage: one-shot-tally [status [--json]|grade [--json]|background <record|complete|list>|todo <add|list|done>|goal <list|show|resume>|version|help]"))
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
	fmt.Fprintln(w, "  one-shot-tally background complete ID")
	fmt.Fprintln(w, "                                  mark complete and wake the originating tmux pane")
	fmt.Fprintln(w, "  one-shot-tally background list  list recorded background jobs and cleanup commands")
	fmt.Fprintln(w, "  one-shot-tally todo add TEXT --context WHY")
	fmt.Fprintln(w, "                                  park discovered out-of-scope work and return to the goal")
	fmt.Fprintln(w, "  one-shot-tally todo list [--all] list open deferred work, or include completed items")
	fmt.Fprintln(w, "  one-shot-tally todo done ID      complete one deferred item")
	fmt.Fprintln(w, "  one-shot-tally goal list [--all] list resumable goals, or all goals")
	fmt.Fprintln(w, "  one-shot-tally goal show ID      print a previous goal")
	fmt.Fprintln(w, "  one-shot-tally goal resume ID    print the exact create_goal handoff")
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
		context := "Finish the latest requested outcome and verify edits. The tally score is advisory. Stay in the current repository unless the user names another target. Before external changes, confirm the target and visible acceptance result. " + subagentGuidance + " " + sparkGuidance + " " + communicationGuidance
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
	prompt = strings.ToLower(prompt)
	for _, marker := range []string{"wrong", "instead", "meant to", "stop ", "stop,", "stop:", "do not ", "don't ", "off the rails", "not the "} {
		if strings.Contains(prompt, marker) {
			return true
		}
	}
	return false
}

func deliveryInvocations(command string) (shipping, deploying bool) {
	if strings.ContainsAny(command, ";&|\n") {
		return false, false
	}
	fields := strings.Fields(command)
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
	isEdit := e.ToolName == "apply_patch" || e.ToolName == "Edit" || e.ToolName == "Write"
	isCommand := isCommandTool(e.ToolName)
	isTest := isCommand && testRE.MatchString(command)
	isShipping, isDeploying := deliveryInvocations(command)
	isProduction := isCommand && !strings.ContainsAny(command, ";&|\n") && (productionRE.MatchString(command) || isShipping || isDeploying)
	isExternalMutation := isCommand && externalMutationRE.MatchString(command)
	isRead := readRE.MatchString(command)
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
	}
	if isDeploying {
		s.DeployAttempts++
	}
	if e.ToolUseID != "" {
		s.Pending[e.ToolUseID] = pendingCall{Test: isTest, Production: isProduction, Revision: s.Revision, StartedAt: time.Now().UTC(), RepeatedTest: repeatedTest, BackgroundRecord: isBackgroundRecord, BackgroundComplete: isBackgroundComplete, TodoAdd: isTodoAdd, TodoDone: isTodoDone, GoalTransition: goalChange, Edit: isEdit, Shipping: isShipping, Deploying: isDeploying}
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
	if len(messages) == 0 {
		return writeJSON(w, hookOutput{})
	}
	return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{HookEventName: "PreToolUse", AdditionalContext: strings.Join(messages, " ")}})
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
		return "the current goal is verified; report success and stop unless a stated requirement remains unmet"
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
	if ok {
		delete(s.Pending, e.ToolUseID)
		passed := responsePassed(e.ToolResponse)
		if pending.Edit && pending.Revision >= s.LastEditResultRevision {
			s.LastEditResultKnown = true
			s.LastEditSucceeded = passed
			s.LastEditResultRevision = pending.Revision
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
			s.LastTestResultKnown = true
			s.LastTestPassed = passed
			if passed {
				s.TestPasses++
				s.VerifiedRevision = pending.Revision
				s.Fingerprints = map[string]int{}
				s.PassiveWaitStreak = 0
			} else {
				s.TestFailures++
			}
		}
		if pending.BackgroundRecord && responsePassed(e.ToolResponse) {
			s.BackgroundRecords++
		}
		if pending.BackgroundComplete && responsePassed(e.ToolResponse) {
			s.BackgroundCompletions++
		}
		if pending.Production && responsePassed(e.ToolResponse) {
			s.ProductionCompletions++
		}
		if pending.Shipping && passed {
			s.ShipCompletions++
		}
		if pending.Shipping {
			s.LastShipResultKnown = true
			s.LastShipSucceeded = passed
		}
		if pending.Deploying {
			s.LastDeployResultKnown = true
			s.LastDeploySucceeded = passed
			if passed {
				s.DeployCompletions++
			}
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
	if ok && responsePassed(e.ToolResponse) && (pending.Edit || pending.Test) {
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
		line = strings.Replace(line, "Goal result: SUCCESS", "Goal result: ACTIVE (recorded work verified)", 1)
		line = strings.Replace(line, "Goal result: NOT VERIFIED", "Goal result: ACTIVE (recorded work not verified)", 1)
	}
	stewardship := ""
	if s.BackgroundRecords > s.BackgroundCompletions {
		stewardship = " Background work remains recorded: do not poll it; its completion command must wake the originating agent, which should resume the task and use the recorded cleanup command."
	}
	closure := ""
	if !e.StopHookActive && !goalActive {
		sparkReview, err := claimSparkRoutingReview(e.SessionID, s)
		if err != nil {
			return err
		}
		closure = closingLoop(s, deployContract) + sparkReview
	}
	if goalActive || !finalPassed(s) {
		return writeJSON(w, hookOutput{SystemMessage: line + ". Advisory: continue with the smallest goal-directed verification step when more work is appropriate; this hook does not block stopping or delivery." + stewardship + closure})
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
	if s.Revision == 0 {
		return ""
	}
	if !finalPassed(s) {
		return " Closing loop: verify the current revision before shipping."
	}
	if !s.LastEditResultKnown || s.LastEditResultRevision != s.Revision {
		return " Closing loop: confirm that the edit completed successfully before shipping."
	}
	if s.LastDeployResultKnown && !s.LastDeploySucceeded {
		return " Closing loop: deploy-it did not complete. Do not rerun it automatically; diagnose the preserved failure before another authorized deployment."
	}
	if s.LastShipResultKnown && !s.LastShipSucceeded {
		return " Closing loop: ship-it did not complete cleanly, and Git may already be shipped. Preserve the failure, diagnose it, and do not rerun a failed deployment automatically."
	}
	if s.DeployCompletions > 0 {
		return " Closing loop: shipping and deployment completed. Confirm the user-visible acceptance result."
	}
	if s.ShipCompletions > 0 {
		if deployContract {
			return " Closing loop: shipping completed. Confirm the ship-it deploy-it handoff and the user-visible acceptance result; do not create deployment trust without exact authorization."
		}
		return " Closing loop: shipping completed. No tracked .deploy-it.json is present, so deployment is intentionally skipped."
	}
	if deployContract {
		return " Closing loop: verified changes are ship-ready. Run ship-it; it will hand off to the tracked deploy-it contract only when that exact contract is already trusted. Confirm the user-visible acceptance result."
	}
	return " Closing loop: verified changes are ship-ready. Run ship-it. No tracked .deploy-it.json means deployment is intentionally skipped."
}

func isAgentSpawn(tool string) bool {
	tool = strings.ToLower(tool)
	return strings.Contains(tool, "spawn_agent") || strings.Contains(tool, "task") && strings.Contains(tool, "agent")
}

func reportLine(s state) string {
	result := "SUCCESS"
	if !finalPassed(s) {
		result = "NOT VERIFIED"
	}
	mode := ""
	coaching := "advisory"
	if s.GoalScoped {
		mode = " | Run mode: /goal (high tool-call volume expected)"
		coaching = "advisory; tool-call volume not scored"
	}
	return fmt.Sprintf("Goal result: %s%s | Tool calls: %d (%d Spark; %s weighted) | Test runs: %d (%d pass, %d fail, %s total, %s redundant) | Delivery actions: %d completed (%d shipped, %d deployed) | Background jobs: %d recorded, %d completed; passive waits: %d | Deferred work: %d parked, %d completed | Coaching signals: %d/100 (%s)", result, mode, s.TotalCalls, s.SparkCalls, formatCallUnits(s.CallCostUnits), completedTests(s), s.TestPasses, s.TestFailures, formatMillis(s.TotalTestMillis), formatMillis(s.RedundantTestMillis), s.ProductionCompletions, s.ShipCompletions, s.DeployCompletions, s.BackgroundRecords, s.BackgroundCompletions, s.PassiveWaits, s.TodosParked, s.TodosCompleted, numericScore(s), coaching)
}

func numericScore(s state) int {
	if s.TestFailures > 0 && !s.LastTestPassed {
		return 0
	}
	if s.Revision > 0 && (s.Tests == 0 || s.VerifiedRevision != s.Revision || !s.LastTestPassed) {
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

func finalPassed(s state) bool {
	if s.LastEditResultKnown && !s.LastEditSucceeded {
		return false
	}
	if s.TestFailures > 0 && !s.LastTestPassed {
		return false
	}
	return s.Revision == 0 || (s.Tests > 0 && s.VerifiedRevision == s.Revision && s.LastTestPassed)
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
	if s.CallCostUnits == 0 && s.TotalCalls > 0 {
		s.CallCostUnits = s.TotalCalls * 4
	}
	return s, nil
}

func emptyState(sessionID, turnID string) state {
	return state{SessionID: sessionID, TurnID: turnID, ToolCounts: map[string]int{}, Fingerprints: map[string]int{}, TestFingerprints: map[string]int{}, Pending: map[string]pendingCall{}}
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
	jobs, err := loadBackgroundJobs()
	if err != nil {
		return err
	}
	jobs[id] = backgroundJob{ID: id, Cleanup: args[2], TmuxTarget: target, SessionID: os.Getenv("ONE_SHOT_SESSION_ID"), TurnID: os.Getenv("ONE_SHOT_TURN_ID"), CreatedAt: time.Now().UTC()}
	if err := saveBackgroundJobs(jobs); err != nil {
		return err
	}
	fmt.Fprintf(w, "Recorded background job %s. Arrange completion with: one-shot-tally background complete %s\n", id, id)
	return nil
}

func completeBackground(args []string, w io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: one-shot-tally background complete ID")
	}
	jobs, err := loadBackgroundJobs()
	if err != nil {
		return err
	}
	job, ok := jobs[args[0]]
	if !ok {
		return fmt.Errorf("background job %q is not recorded", args[0])
	}
	job.CompletedAt = time.Now().UTC()
	jobs[job.ID] = job
	if err := saveBackgroundJobs(jobs); err != nil {
		return err
	}
	if job.TmuxTarget != "" {
		message := fmt.Sprintf("Background job %s completed. Resume the originating task now; cleanup record: %s", job.ID, singleLine(job.Cleanup))
		if err := exec.Command("tmux", "send-keys", "-l", "-t", job.TmuxTarget, message).Run(); err != nil {
			return fmt.Errorf("job recorded complete, but tmux wake-up failed: %w", err)
		}
		if err := exec.Command("tmux", "send-keys", "-t", job.TmuxTarget, "Enter").Run(); err != nil {
			return fmt.Errorf("job recorded complete, but tmux Enter failed: %w", err)
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
	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "lifetime.json" || entry.Name() == "background-jobs.json" || entry.Name() == "todos.json" {
			continue
		}
		info, err := entry.Info()
		if err == nil {
			files = append(files, candidate{filepath.Join(dir, entry.Name()), info.ModTime()})
		}
	}
	if len(files) == 0 {
		return errors.New("no tally state found")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	b, err := os.ReadFile(files[0].path)
	if err != nil {
		return err
	}
	var s state
	if _, err := decodeState(b, &s); err != nil {
		return err
	}
	life, _ := loadLifetime()
	if asJSON {
		return writeJSON(os.Stdout, map[string]any{"state": s, "success": finalPassed(s), "coaching_score": numericScore(s), "report": reportLine(s), "lifetime": life})
	}
	fmt.Println(reportLine(s))
	if life.Runs > 0 {
		fmt.Printf("Lifetime: Verified goals: %d/%d | Coaching average: %.1f/100 | Tests: %d (%d failed, %s total) | Tool calls: %d\n", life.VerifiedRuns, life.Runs, life.AverageScore, life.TotalTests, life.TotalTestFailures, formatMillis(life.TotalTestMillis), life.TotalToolCalls)
	}
	return nil
}

func recordLifetime(s state) error {
	life, err := loadLifetime()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	life.Runs++
	life.TotalScore += numericScore(s)
	life.AverageScore = float64(life.TotalScore) / float64(life.Runs)
	if s.Revision == 0 || (s.VerifiedRevision == s.Revision && s.LastTestPassed) {
		life.VerifiedRuns++
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
func fatal(err error)                        { fmt.Fprintln(os.Stderr, "one-shot-tally:", err); os.Exit(1) }
