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

const binaryVersion = "1.8.6"

var (
	// Test runners must begin a shell command segment. Matching a bare "test"
	// token anywhere lets harmless commands such as "echo test" forge a passing
	// verification for the current revision.
	testRE                 = regexp.MustCompile(`(?i)(^|[;&|])\s*([a-z_][a-z0-9_]*=\S+\s+)*(pytest|go\s+test|cargo\s+test|npm\s+(run\s+)?test|pnpm\s+(run\s+)?test|yarn\s+test|bun\s+test|rspec|phpunit|gradle\w*\s+test|mvn\w*\s+test|make\s+(test|check|verify)|(verify|check)(\.sh)?|(\./|[^\s;&|]+/)(test|tests|verify|check)(\.sh)?)([;&|\s]|$)`)
	readRE                 = regexp.MustCompile(`(?i)^\s*(rg|grep|find|fd|sed\s+-n|head|tail|ls|stat|cat\s|git\s+(status|diff|log|show|branch|rev-parse|worktree\s+list))`)
	productionRE           = regexp.MustCompile("(?i)(git\\s+push|ship-it\\s*$|(depl" + "oy|rele" + "ase)[^;&|]*(prod|production)|prod\\s+depl" + "oy|kubectl\\s+apply|docker\\s+stack\\s+depl" + "oy|terraform\\s+apply)")
	passiveWaitRE          = regexp.MustCompile(`(?i)^\s*(sleep\b|watch\b|tail\s+-f\b|while\b.*\bsleep\b|until\b.*\bsleep\b|tmux\s+(capture-pane|list-panes|list-sessions|has-session)\b)`)
	detachedTmuxRE         = regexp.MustCompile(`(?i)\btmux\s+(new-session|new)\b[^\n]*(\s-d\b|-d\s)`)
	backgroundRecordRE     = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+record\b`)
	backgroundCompleteRE   = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+background\s+complete\b`)
	todoAddRE              = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+add\b`)
	todoDoneRE             = regexp.MustCompile(`(?i)(^|[;&|]\s*)(\S*/)?one-shot-tally\s+todo\s+done\b`)
	shipGateRE             = regexp.MustCompile(`(?i)\bship-it\b|(^|[/\s` + "`" + `])ship(\.project)?\.sh\b`)
	deployGateRE           = regexp.MustCompile(`(?i)\bdeploy-it\b|(^|[/\s` + "`" + `])deploy\.sh\b|\.woodpecker\.ya?ml|\.github/workflows|workflow_dispatch|woodpecker[^\n]*(deploy|trigger|push|build|success|converg)|(deploy|trigger|push|build|success|converg)[^\n]*woodpecker`)
	directContractDeleteRE = regexp.MustCompile(`(?i)(^|[;&|])\s*(git\s+rm|rm)\b[^;\n]*(ship-it|deploy-it|ship(\.project)?\.sh|deploy\.sh|\.woodpecker\.ya?ml|\.github/workflows)`)
	dangerousGitCommandRE  = regexp.MustCompile(`(?i)(^|[;&|]\s*)\s*([A-Za-z_][A-Za-z0-9_]*=\S+\s+)*(\S*/)?git(\s+(-C|--git-dir|--work-tree)\s+("[^"]*"|'[^']*'|\S+))*\s+(filter-branch|filter-repo|reset\s+--hard|clean(\s|$)|reflog\s+(expire|delete)|update-ref\b[^;&|\n]*\s-d(\s|$)|worktree\s+(remove|move|prune|repair)(\s|$)|gc\b[^;&|\n]*--prune(=|\s+)now|prune(\s|$)|replace(\s|$))`)
	gitMetadataPathRE      = regexp.MustCompile(`(?i)(^|[/\\\s"'=])\.git([/\\\s"'=]|$)`)
	gitMetadataWriteRE     = regexp.MustCompile(`(?i)(^|[;&|]\s*)\s*(rm|rmdir|unlink|mv|cp|chmod|chown|touch|truncate|install|mkdir|ln|tee|dd)\b|(^|[;&|]\s*)\s*find\b[^;&|\n]*\s-delete(\s|$)|>>?\s*[^;&|\n]*\.git([/\\\s"']|$)`)
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
}

type pendingCall struct {
	Test               bool      `json:"test"`
	Production         bool      `json:"production"`
	Revision           int       `json:"revision"`
	StartedAt          time.Time `json:"started_at"`
	RepeatedTest       bool      `json:"repeated_test"`
	BackgroundRecord   bool      `json:"background_record"`
	BackgroundComplete bool      `json:"background_complete"`
	ContractRecovery   bool      `json:"contract_recovery"`
	TodoAdd            bool      `json:"todo_add"`
	TodoDone           bool      `json:"todo_done"`
	GoalTransition     string    `json:"goal_transition,omitempty"`
}

type state struct {
	StateVersion                int                    `json:"state_version"`
	SessionID                   string                 `json:"session_id"`
	TurnID                      string                 `json:"turn_id"`
	UpdatedAt                   time.Time              `json:"updated_at"`
	TotalCalls                  int                    `json:"total_calls"`
	CallCostUnits               int                    `json:"call_cost_units"`
	SparkCalls                  int                    `json:"spark_calls"`
	Tests                       int                    `json:"tests"`
	TestPasses                  int                    `json:"test_passes"`
	TestFailures                int                    `json:"test_failures"`
	TotalTestMillis             int64                  `json:"total_test_millis"`
	MaxTestMillis               int64                  `json:"max_test_millis"`
	RedundantTestMillis         int64                  `json:"redundant_test_millis"`
	Revision                    int                    `json:"revision"`
	VerifiedRevision            int                    `json:"verified_revision"`
	InspectionStreak            int                    `json:"inspection_streak"`
	MaxInspectionStreak         int                    `json:"max_inspection_streak"`
	RepeatedWarnings            int                    `json:"repeated_warnings"`
	ProductionBlocks            int                    `json:"production_blocks"`
	ProductionCompletions       int                    `json:"production_completions"`
	BackgroundRecords           int                    `json:"background_records"`
	BackgroundCompletions       int                    `json:"background_completions"`
	PassiveWaits                int                    `json:"passive_waits"`
	DeliveryContractFailures    int                    `json:"delivery_contract_failures"`
	DeliveryContractRecoveries  int                    `json:"delivery_contract_recoveries"`
	OpenDeliveryContractFailure bool                   `json:"open_delivery_contract_failure"`
	GitMetadataBlocks           int                    `json:"git_metadata_blocks"`
	TodosParked                 int                    `json:"todos_parked"`
	TodosCompleted              int                    `json:"todos_completed"`
	ToolCounts                  map[string]int         `json:"tool_counts"`
	Fingerprints                map[string]int         `json:"fingerprints"`
	TestFingerprints            map[string]int         `json:"test_fingerprints"`
	Pending                     map[string]pendingCall `json:"pending"`
	LastTestPassed              bool                   `json:"last_test_passed"`
	LastTestResultKnown         bool                   `json:"last_test_result_known"`
	RecordedInLifetime          bool                   `json:"recorded_in_lifetime"`
	GoalScoped                  bool                   `json:"goal_scoped"`
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

type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
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
		case "version":
			printVersion(os.Stdout)
			return
		case "help", "-h", "--help":
			printHelp(os.Stdout)
			return
		default:
			fatal(fmt.Errorf("usage: one-shot-tally [status [--json]|grade [--json]|background <record|complete|list>|todo <add|list|done>|version|help]"))
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
	fmt.Fprintln(w, "  one-shot-tally version          show the version")
	fmt.Fprintln(w, "  one-shot-tally help|-h|--help   show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Agent guidance:")
	fmt.Fprintln(w, "  Delegate independent, bounded, low-risk work to spark_worker subagents.")
	fmt.Fprintln(w, "  Keep architecture, authorization, integration, and final acceptance with the primary agent.")
	fmt.Fprintln(w, "  Spark calls count as 0.25 normal calls for tool-pressure scoring; tests and correctness gates are never discounted.")
	fmt.Fprintln(w, "  During /goal work, high tool-call volume is expected and is not scored; other coaching signals still apply.")
	fmt.Fprintln(w, "  Five tests is a pacing guideline, not a hard stop; required verification may continue with a score penalty.")
	fmt.Fprintln(w, "  Record detached jobs with cleanup and wake-up data; passive polling and waiting reduce the score.")
	fmt.Fprintln(w, "  Successful verified production calls are reported and earn one capped outcome credit.")
	fmt.Fprintln(w, "  Removing a ship or automated-deploy gate without a same-edit replacement is denied.")
	fmt.Fprintln(w, "  Destructive Git-history, worktree, and direct .git mutations are denied. Use read-only Git inspection or ship-it.")
	fmt.Fprintln(w, "  Do not optimize the score by doing nothing; complete the requested outcome and recover from correctable mistakes.")
	fmt.Fprintln(w, "  F is reserved for failed or unverified outcomes; verified completion has a D/50 floor even when inefficient.")
	fmt.Fprintln(w, "  Park useful side discoveries in the durable TODO list; a verified current outcome earns a small capped reward.")
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
		context := "Complete and verify the requested goal. Goal success is the only completion metric. Coaching signals can improve the path, but they never turn verified success into failure or justify stopping early. Use the smallest goal-directed next step. Do not manufacture edits, tests, or scope to improve a signal. Park useful side work with `one-shot-tally todo add TEXT --context WHY`, then return to the goal. Delegate bounded, low-risk work to spark_worker subagents when useful. Keep architecture, authorization, integration, and final acceptance with the primary agent. Preserve ship and deploy gates. For long work, record cleanup and a completion wake-up instead of polling. Efficiency never outranks completion or correctness."
		goalActive, err := sessionGoalActive(e.SessionID)
		if err != nil {
			return err
		}
		if goalActive {
			context += " This is a /goal continuation. High tool-call volume is expected and does not reduce the coaching score. Keep work tied to the objective. Park unrelated work."
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: context,
		}})
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
	isProduction := isCommand && productionRE.MatchString(command)
	isRead := readRE.MatchString(command)
	isPassiveWait := passiveWait(e, command)
	isBackgroundRecord := isCommand && backgroundRecordRE.MatchString(command)
	isBackgroundComplete := isCommand && backgroundCompleteRE.MatchString(command)
	isTodoAdd := isCommand && todoAddRE.MatchString(command)
	isTodoDone := isCommand && todoDoneRE.MatchString(command)
	removedGates := removedDeliveryGates(e.ToolInput)
	contractFailure := (isEdit && len(removedGates) > 0) || (isCommand && directContractDeleteRE.MatchString(command))
	gitMetadataMutation := (isCommand && commandMutatesGitMetadata(command)) || (isEdit && editTargetsGitMetadata(e.ToolInput))

	previousCostUnits := s.CallCostUnits
	s.TotalCalls++
	callUnits := 4
	if isSparkCall(e) {
		callUnits = 1
		s.SparkCalls++
	}
	s.CallCostUnits += callUnits
	s.ToolCounts[e.ToolName]++
	fp := fingerprint(e.ToolName, e.ToolInput)
	s.Fingerprints[fp]++
	repeats := s.Fingerprints[fp]
	if gitMetadataMutation {
		s.GitMetadataBlocks++
		if err := save(p, s); err != nil {
			return err
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: "Blocked: automated Git metadata and history destruction is disabled. This action can rewrite or remove .git state. Use read-only Git inspection or ordinary ship-it. Perform exceptional history or worktree surgery manually outside the agent hook after making a backup.",
		}})
	}
	if contractFailure {
		s.DeliveryContractFailures++
		s.OpenDeliveryContractFailure = true
		if err := save(p, s); err != nil {
			return err
		}
		detail := strings.Join(removedGates, " and ")
		if detail == "" {
			detail = "delivery contract"
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: "Blocked: this action removes the " + detail + " without an equivalent same-edit replacement. Nothing changed. Continue now with a corrected contract-preserving edit, then verify the final revision; successful recovery can ship, but this attempt retains a score penalty.",
		}})
	}
	if isEdit {
		s.Revision++
		s.InspectionStreak = 0
	} else if isRead {
		s.InspectionStreak++
		if s.InspectionStreak > s.MaxInspectionStreak {
			s.MaxInspectionStreak = s.InspectionStreak
		}
	}
	if isProduction && (unresolvedDeliveryContract(s) || s.VerifiedRevision != s.Revision || !s.LastTestPassed) {
		s.ProductionBlocks++
		if err := save(p, s); err != nil {
			return err
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: productionBlockReason(s),
		}})
	}
	repeatedTest := false
	if isTest {
		s.Tests++
		s.TestFingerprints[fp]++
		repeatedTest = s.TestFingerprints[fp] > 1
	}
	if isPassiveWait {
		s.PassiveWaits++
	}
	if e.ToolUseID != "" {
		s.Pending[e.ToolUseID] = pendingCall{Test: isTest, Production: isProduction, Revision: s.Revision, StartedAt: time.Now().UTC(), RepeatedTest: repeatedTest, BackgroundRecord: isBackgroundRecord, BackgroundComplete: isBackgroundComplete, ContractRecovery: isEdit && unresolvedDeliveryContract(s), TodoAdd: isTodoAdd, TodoDone: isTodoDone, GoalTransition: goalChange}
	}
	var messages []string
	if goalChange == "start" {
		messages = append(messages, "Goal mode started. High tool-call volume is expected and does not reduce the coaching score. Keep each call tied to the objective. Park unrelated work.")
	}
	if isPassiveWait {
		messages = append(messages, "Progress check: passive waiting adds no evidence. Detach long work, record its cleanup and wake-up target, then continue another goal step or stop.")
	}
	if isCommand && detachedTmuxRE.MatchString(command) && !isBackgroundRecord {
		messages = append(messages, "Progress check: record this detached job, its cleanup command, and its tmux pane. Completion can then wake the agent without polling.")
	}
	if repeats == 3 {
		s.RepeatedWarnings++
		messages = append(messages, fmt.Sprintf("Progress check: %s received the same input 3 times. Use the existing result. Next: %s", e.ToolName, nextAction(s)))
	}
	if s.InspectionStreak == 8 {
		messages = append(messages, fmt.Sprintf("Progress check: 8 consecutive inspections with %d edits and %d tests this turn. Next: %s", s.Revision, s.Tests, nextAction(s)))
	}
	thresholds := []int{12, 20, 30}
	if s.GoalScoped {
		thresholds = []int{40, 80, 120}
	}
	for _, threshold := range thresholds {
		if previousCostUnits < threshold*4 && s.CallCostUnits >= threshold*4 {
			if s.GoalScoped {
				messages = append(messages, fmt.Sprintf("Goal progress check: %d tool calls (%s weighted after the Spark discount), %d edits, %d tests, and a longest inspection streak of %d. High tool-call volume is expected and is not scored. Keep calls tied to the objective. Repetition, passive waits, redundant tests, and unrelated scope still affect coaching. Next: %s", s.TotalCalls, formatCallUnits(s.CallCostUnits), s.Revision, s.Tests, s.MaxInspectionStreak, nextAction(s)))
			} else {
				messages = append(messages, fmt.Sprintf("Progress check: %d tool calls (%s weighted after the Spark discount), %d edits, %d tests, and a longest inspection streak of %d. Next: %s", s.TotalCalls, formatCallUnits(s.CallCostUnits), s.Revision, s.Tests, s.MaxInspectionStreak, nextAction(s)))
			}
		}
	}
	if isTest && (s.Tests == 4 || s.Tests == 5) {
		messages = append(messages, fmt.Sprintf("Progress check: this is test run %d of the ordinary 5-run guide. Next: %s", s.Tests, nextAction(s)))
	}
	if isTest && s.Tests > 5 {
		messages = append(messages, fmt.Sprintf("Progress check: test run %d exceeds the ordinary 5-run guide. Continue required verification, but avoid redundant reruns. Next: %s", s.Tests, nextAction(s)))
	}
	if err := save(p, s); err != nil {
		return err
	}
	if len(messages) == 0 {
		return writeJSON(w, hookOutput{})
	}
	return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{HookEventName: "PreToolUse", AdditionalContext: strings.Join(messages, " ")}})
}

func productionBlockReason(s state) string {
	if unresolvedDeliveryContract(s) {
		return "Production action blocked: the denied delivery-contract removal is not yet recovered. Continue with one corrected contract-preserving edit, then verify that final revision and retry."
	}
	return "Production action blocked: the current code revision does not have a recorded passing verification. Run the final required check once, then retry."
}

func unresolvedDeliveryContract(s state) bool {
	return s.OpenDeliveryContractFailure
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
		if pending.Test {
			duration := testDurationMillis(e.ToolResponse, pending.StartedAt)
			s.TotalTestMillis += duration
			if duration > s.MaxTestMillis {
				s.MaxTestMillis = duration
			}
			if pending.RepeatedTest {
				s.RedundantTestMillis += duration
			}
			passed := responsePassed(e.ToolResponse)
			s.LastTestResultKnown = true
			s.LastTestPassed = passed
			if passed {
				s.TestPasses++
				s.VerifiedRevision = pending.Revision
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
		if pending.ContractRecovery && responsePassed(e.ToolResponse) {
			s.DeliveryContractRecoveries++
			s.OpenDeliveryContractFailure = false
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
	return writeJSON(w, hookOutput{})
}

func stop(e event, w io.Writer) error {
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
	line := reportLine(s)
	stewardship := ""
	if s.BackgroundRecords > s.BackgroundCompletions {
		stewardship = " Background work remains recorded: do not poll it; its completion command must wake the originating agent, which should resume the task and use the recorded cleanup command."
	}
	hasWork := s.Tests > 0 || s.Revision > 0
	if hasWork && !finalPassed(s) && !e.StopHookActive {
		return writeJSON(w, hookOutput{Decision: "block", Reason: "The requested goal is not verified yet. Continue with the smallest goal-directed step. Fix the known failure or run the required verification. Do not expand scope; park useful side work for later." + stewardship})
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
	if hasWork && !strings.Contains(e.LastAssistantMsg, "Goal result: SUCCESS") && !e.StopHookActive {
		return writeJSON(w, hookOutput{Decision: "block", Reason: "Append this mechanical verification line to the final response without additional investigation: " + line + stewardship})
	}
	return writeJSON(w, hookOutput{SystemMessage: line + stewardship})
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
	return fmt.Sprintf("Goal result: %s%s | Tool calls: %d (%d Spark; %s weighted) | Test runs: %d (%d pass, %d fail, %s total, %s redundant) | Production: %d blocked, %d completed | Background jobs: %d recorded, %d completed; passive waits: %d | Deferred work: %d parked, %d completed | Git metadata: %d blocked | Delivery contract: %d blocked, %d recovered | Coaching signals: %d/100 (%s)", result, mode, s.TotalCalls, s.SparkCalls, formatCallUnits(s.CallCostUnits), completedTests(s), s.TestPasses, s.TestFailures, formatMillis(s.TotalTestMillis), formatMillis(s.RedundantTestMillis), s.ProductionBlocks, s.ProductionCompletions, s.BackgroundRecords, s.BackgroundCompletions, s.PassiveWaits, s.TodosParked, s.TodosCompleted, s.GitMetadataBlocks, s.DeliveryContractFailures, s.DeliveryContractRecoveries, numericScore(s), coaching)
}

func numericScore(s state) int {
	if unresolvedDeliveryContract(s) {
		return 0
	}
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
	productionPenalty := minInt(30, s.ProductionBlocks*15)
	if finalPassed(s) {
		productionPenalty = minInt(15, productionPenalty)
	}
	score -= productionPenalty
	score -= minInt(30, s.DeliveryContractFailures*15)
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
	if unresolvedDeliveryContract(s) || (s.TestFailures > 0 && !s.LastTestPassed) {
		return false
	}
	return s.Revision == 0 || (s.Tests > 0 && s.VerifiedRevision == s.Revision && s.LastTestPassed)
}

func removedDeliveryGates(raw json.RawMessage) []string {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var removed, added []string
	if patch, ok := fields["patch"].(string); ok {
		for _, line := range strings.Split(patch, "\n") {
			switch {
			case strings.HasPrefix(line, "*** Delete File:"):
				removed = append(removed, strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File:")))
			case strings.HasPrefix(line, "*** Add File:"):
				added = append(added, strings.TrimSpace(strings.TrimPrefix(line, "*** Add File:")))
			case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
				removed = append(removed, strings.TrimPrefix(line, "-"))
			case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
				added = append(added, strings.TrimPrefix(line, "+"))
			}
		}
	}
	if oldText, ok := fields["old_string"].(string); ok {
		removed = append(removed, oldText)
	}
	if newText, ok := fields["new_string"].(string); ok {
		added = append(added, newText)
	}
	oldText, newText := strings.Join(removed, "\n"), strings.Join(added, "\n")
	var missing []string
	if shipGateRE.MatchString(oldText) && !shipGateRE.MatchString(newText) {
		missing = append(missing, "ship gate")
	}
	if deployGateRE.MatchString(oldText) && !deployGateRE.MatchString(newText) {
		missing = append(missing, "automated deploy gate")
	}
	return missing
}

func commandMutatesGitMetadata(command string) bool {
	if dangerousGitCommandRE.MatchString(command) {
		return true
	}
	return gitMetadataPathRE.MatchString(command) && gitMetadataWriteRE.MatchString(command)
}

func editTargetsGitMetadata(raw json.RawMessage) bool {
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, key := range []string{"path", "file_path"} {
		if path, ok := fields[key].(string); ok && pathContainsGitMetadata(path) {
			return true
		}
	}
	patch, _ := fields["patch"].(string)
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:", "--- a/", "+++ b/"} {
			if strings.HasPrefix(line, prefix) && pathContainsGitMetadata(strings.TrimSpace(strings.TrimPrefix(line, prefix))) {
				return true
			}
		}
	}
	return false
}

func pathContainsGitMetadata(path string) bool {
	path = strings.ReplaceAll(strings.Trim(path, " \t\r\n\"'"), "\\", "/")
	for _, part := range strings.Split(path, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
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
			if pending.Production && s.ProductionBlocks > 0 {
				s.ProductionBlocks--
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
