package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// Test runners must begin a shell command segment. Matching a bare "test"
	// token anywhere lets harmless commands such as "echo test" forge a passing
	// verification for the current revision.
	testRE       = regexp.MustCompile(`(?i)(^|[;&|])\s*([a-z_][a-z0-9_]*=\S+\s+)*(pytest|go\s+test|cargo\s+test|npm\s+(run\s+)?test|pnpm\s+(run\s+)?test|yarn\s+test|bun\s+test|rspec|phpunit|gradle\w*\s+test|mvn\w*\s+test|make\s+(test|check|verify)|(verify|check)(\.sh)?|(\./|[^\s;&|]+/)(test|tests|verify|check)(\.sh)?)([;&|\s]|$)`)
	readRE       = regexp.MustCompile(`(?i)^\s*(rg|grep|find|fd|sed\s+-n|head|tail|ls|stat|cat\s|git\s+(status|diff|log|show|branch|rev-parse|worktree\s+list))`)
	productionRE = regexp.MustCompile("(?i)(git\\s+push|ship-it\\s*$|(depl" + "oy|rele" + "ase)[^;&|]*(prod|production)|prod\\s+depl" + "oy|kubectl\\s+apply|docker\\s+stack\\s+depl" + "oy|terraform\\s+apply)")
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
	Test         bool      `json:"test"`
	Production   bool      `json:"production"`
	Revision     int       `json:"revision"`
	StartedAt    time.Time `json:"started_at"`
	RepeatedTest bool      `json:"repeated_test"`
}

type state struct {
	StateVersion        int                    `json:"state_version"`
	SessionID           string                 `json:"session_id"`
	TurnID              string                 `json:"turn_id"`
	UpdatedAt           time.Time              `json:"updated_at"`
	TotalCalls          int                    `json:"total_calls"`
	CallCostUnits       int                    `json:"call_cost_units"`
	SparkCalls          int                    `json:"spark_calls"`
	Tests               int                    `json:"tests"`
	TestPasses          int                    `json:"test_passes"`
	TestFailures        int                    `json:"test_failures"`
	TotalTestMillis     int64                  `json:"total_test_millis"`
	MaxTestMillis       int64                  `json:"max_test_millis"`
	RedundantTestMillis int64                  `json:"redundant_test_millis"`
	Revision            int                    `json:"revision"`
	VerifiedRevision    int                    `json:"verified_revision"`
	InspectionStreak    int                    `json:"inspection_streak"`
	MaxInspectionStreak int                    `json:"max_inspection_streak"`
	RepeatedWarnings    int                    `json:"repeated_warnings"`
	ProductionBlocks    int                    `json:"production_blocks"`
	ToolCounts          map[string]int         `json:"tool_counts"`
	Fingerprints        map[string]int         `json:"fingerprints"`
	TestFingerprints    map[string]int         `json:"test_fingerprints"`
	Pending             map[string]pendingCall `json:"pending"`
	LastTestPassed      bool                   `json:"last_test_passed"`
	LastTestResultKnown bool                   `json:"last_test_result_known"`
	RecordedInLifetime  bool                   `json:"recorded_in_lifetime"`
}

type lifetime struct {
	Runs              int            `json:"runs"`
	TotalScore        int            `json:"total_score"`
	AverageScore      float64        `json:"average_score"`
	VerifiedRuns      int            `json:"verified_runs"`
	TotalToolCalls    int            `json:"total_tool_calls"`
	TotalTests        int            `json:"total_tests"`
	TotalTestFailures int            `json:"total_test_failures"`
	TotalTestMillis   int64          `json:"total_test_millis"`
	Grades            map[string]int `json:"grades"`
	UpdatedAt         time.Time      `json:"updated_at"`
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
		case "version":
			fmt.Println("one-shot-tally 1.1.0")
			return
		case "help", "-h", "--help":
			printHelp(os.Stdout)
			return
		default:
			fatal(fmt.Errorf("usage: one-shot-tally [status [--json]|grade [--json]|version|help]"))
		}
	}
	if err := runHook(os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "one-shot-tally - mechanical one-shot delivery hook")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  one-shot-tally                  process a hook event from stdin")
	fmt.Fprintln(w, "  one-shot-tally status [--json]  show the latest run and lifetime totals")
	fmt.Fprintln(w, "  one-shot-tally grade [--json]   alias for status")
	fmt.Fprintln(w, "  one-shot-tally version          show the version")
	fmt.Fprintln(w, "  one-shot-tally help|-h|--help   show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Agent guidance:")
	fmt.Fprintln(w, "  Delegate independent, bounded, low-risk work to spark_worker subagents.")
	fmt.Fprintln(w, "  Keep architecture, authorization, integration, and final acceptance with the primary agent.")
	fmt.Fprintln(w, "  Spark calls count as 0.25 normal calls for tool-pressure scoring; tests and correctness gates are never discounted.")
}

func runHook(r io.Reader, w io.Writer) error {
	var e event
	if err := json.NewDecoder(r).Decode(&e); err != nil {
		return err
	}
	switch e.HookEventName {
	case "SessionStart":
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: "Use one-shot delivery: gather evidence once, implement a substantial pass, keep output bounded, and avoid repeated agentic loops. Delegate independent, bounded, low-risk work to spark_worker subagents when possible; their tool-pressure cost is discounted. Keep architecture, authorization, integration, and final acceptance with the primary agent. The compiled one-shot-tally hook mechanically counts tools, revisions, tests, and verification state.",
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
	p, s, err := load(e)
	if err != nil {
		return err
	}
	command := commandFrom(e.ToolInput)
	isEdit := e.ToolName == "apply_patch" || e.ToolName == "Edit" || e.ToolName == "Write"
	isCommand := isCommandTool(e.ToolName)
	isTest := isCommand && testRE.MatchString(command)
	isProduction := isCommand && productionRE.MatchString(command)
	isRead := readRE.MatchString(command)

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
	if e.ToolUseID != "" {
		s.Pending[e.ToolUseID] = pendingCall{Test: isTest, Production: isProduction, Revision: s.Revision, StartedAt: time.Now().UTC(), RepeatedTest: repeatedTest}
	}

	if isProduction && (s.VerifiedRevision != s.Revision || !s.LastTestPassed) {
		s.ProductionBlocks++
		if err := save(p, s); err != nil {
			return err
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: "Production action blocked: the current code revision does not have a recorded passing verification. Run the final required check once, then retry.",
		}})
	}
	if isTest && s.Tests > 5 {
		if err := save(p, s); err != nil {
			return err
		}
		return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{
			HookEventName: "PreToolUse", PermissionDecision: "deny",
			PermissionDecisionReason: fmt.Sprintf("Test run %d exceeds the five-run budget. Stop and reassess the diagnosis and change boundary before requesting an explicit exception.", s.Tests),
		}})
	}

	var messages []string
	if repeats == 3 {
		s.RepeatedWarnings++
		messages = append(messages, fmt.Sprintf("Observed: %s received the same input 3 times, so the latest call added no new evidence. Next: %s", e.ToolName, nextAction(s)))
	}
	if s.InspectionStreak == 8 {
		messages = append(messages, fmt.Sprintf("Observed: 8 consecutive inspections with %d edits and %d tests this turn. Next: %s", s.Revision, s.Tests, nextAction(s)))
	}
	for _, threshold := range []int{12, 20, 30} {
		if previousCostUnits < threshold*4 && s.CallCostUnits >= threshold*4 {
			messages = append(messages, fmt.Sprintf("Observed: %d tool calls (%s weighted after the Spark discount), %d edits, %d tests, and a longest inspection streak of %d. Next: %s", s.TotalCalls, formatCallUnits(s.CallCostUnits), s.Revision, s.Tests, s.MaxInspectionStreak, nextAction(s)))
		}
	}
	if isTest && (s.Tests == 4 || s.Tests == 5) {
		messages = append(messages, fmt.Sprintf("Observed: this is test run %d of the ordinary 5-run maximum. Next: %s", s.Tests, nextAction(s)))
	}
	if err := save(p, s); err != nil {
		return err
	}
	if len(messages) == 0 {
		return writeJSON(w, hookOutput{})
	}
	return writeJSON(w, hookOutput{HookSpecificOutput: &hookSpecificOutput{HookEventName: "PreToolUse", AdditionalContext: strings.Join(messages, " ")}})
}

func nextAction(s state) string {
	switch {
	case s.LastTestResultKnown && !s.LastTestPassed:
		return "use the failure output to diagnose one cause, make one complete correction, then rerun only the affected check and final contract"
	case s.Revision == 0:
		return "summarize the evidence already gathered, choose the highest-confidence change, and make one coherent edit; inspect again only for a specific unanswered question"
	case s.VerifiedRevision == s.Revision && s.LastTestPassed:
		return "the current revision is verified; stop investigating and report the completed outcome unless a stated requirement is still unmet"
	default:
		return "finish the current change boundary, review it once, and run the narrow check plus required final contract without another inspection loop"
	}
}

func postToolUse(e event, w io.Writer) error {
	p, s, err := load(e)
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
	}
	if err := save(p, s); err != nil {
		return err
	}
	return writeJSON(w, hookOutput{})
}

func stop(e event, w io.Writer) error {
	p, s, err := load(e)
	if err != nil {
		return err
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
	line := reportLine(s)
	if (s.Tests > 0 || s.Revision > 0) && !strings.Contains(e.LastAssistantMsg, "Discipline score:") && !e.StopHookActive {
		return writeJSON(w, hookOutput{Decision: "block", Reason: "Append this mechanical verification line to the final response without additional investigation: " + line})
	}
	return writeJSON(w, hookOutput{SystemMessage: line})
}

func reportLine(s state) string {
	result := "PASS"
	if s.TestFailures > 0 && !s.LastTestPassed {
		result = "FAIL"
	}
	return fmt.Sprintf("Tool calls: %d (%d Spark; %s weighted) | Test runs: %d (%d pass, %d fail, %s total, %s redundant) | Final result: %s | Discipline score: %s (%d/100)", s.TotalCalls, s.SparkCalls, formatCallUnits(s.CallCostUnits), s.Tests, s.TestPasses, s.TestFailures, formatMillis(s.TotalTestMillis), formatMillis(s.RedundantTestMillis), result, grade(s), numericScore(s))
}

func grade(s state) string {
	if numericScore(s) < 50 {
		return "F"
	}
	if numericScore(s) < 65 {
		return "D"
	}
	if numericScore(s) < 80 {
		return "C"
	}
	if numericScore(s) < 90 {
		return "B"
	}
	if s.Tests > 5 {
		return "D"
	}
	if s.Tests == 5 || s.RepeatedWarnings > 1 || s.MaxInspectionStreak >= 12 {
		return "C"
	}
	if s.Tests == 1 || s.Tests == 4 || s.RepeatedWarnings == 1 || s.MaxInspectionStreak >= 8 {
		return "B"
	}
	return "A"
}

func numericScore(s state) int {
	if s.ProductionBlocks > 0 || (s.TestFailures > 0 && !s.LastTestPassed) {
		return 0
	}
	if s.Revision > 0 && (s.Tests == 0 || s.VerifiedRevision != s.Revision || !s.LastTestPassed) {
		return 25
	}
	score := 100
	switch {
	case s.Tests > 5:
		score -= 35 + (s.Tests-6)*5
	case s.Tests == 5:
		score -= 20
	case s.Tests == 4:
		score -= 10
	case s.Tests == 1 && s.Revision > 0:
		score -= 8
	}
	score -= minInt(20, s.RepeatedWarnings*8)
	if s.MaxInspectionStreak >= 8 {
		score -= minInt(15, s.MaxInspectionStreak-5)
	}
	effectiveCalls := (s.CallCostUnits + 3) / 4
	if effectiveCalls > 30 {
		score -= minInt(15, (effectiveCalls-30)/3+1)
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
		return 0
	}
	return score
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

func load(e event) (string, state, error) {
	p, err := statePath(e)
	if err != nil {
		return "", state{}, err
	}
	s := state{SessionID: e.SessionID, TurnID: e.TurnID, ToolCounts: map[string]int{}, Fingerprints: map[string]int{}, TestFingerprints: map[string]int{}, Pending: map[string]pendingCall{}}
	b, err := os.ReadFile(p)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", state{}, err
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
	return p, s, nil
}

func save(path string, s state) error {
	s.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func statePath(e event) (string, error) {
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
	sum := sha256.Sum256([]byte(e.SessionID + ":" + e.TurnID))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".json"), nil
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
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "lifetime.json" {
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
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	life, _ := loadLifetime()
	if asJSON {
		return writeJSON(os.Stdout, map[string]any{"state": s, "grade": grade(s), "score": numericScore(s), "report": reportLine(s), "lifetime": life})
	}
	fmt.Println(reportLine(s))
	if life.Runs > 0 {
		fmt.Printf("Lifetime: %d runs | Average: %.1f/100 | Verified: %d/%d | Tests: %d (%d failed, %s total) | Tool calls: %d\n", life.Runs, life.AverageScore, life.VerifiedRuns, life.Runs, life.TotalTests, life.TotalTestFailures, formatMillis(life.TotalTestMillis), life.TotalToolCalls)
	}
	return nil
}

func recordLifetime(s state) error {
	life, err := loadLifetime()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if life.Grades == nil {
		life.Grades = map[string]int{}
	}
	life.Runs++
	life.TotalScore += numericScore(s)
	life.AverageScore = float64(life.TotalScore) / float64(life.Runs)
	if s.Revision == 0 || (s.VerifiedRevision == s.Revision && s.LastTestPassed) {
		life.VerifiedRuns++
	}
	life.TotalToolCalls += s.TotalCalls
	life.TotalTests += s.Tests
	life.TotalTestFailures += s.TestFailures
	life.TotalTestMillis += s.TotalTestMillis
	life.Grades[grade(s)]++
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
