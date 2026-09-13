package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	negativeDeliveryRE   = regexp.MustCompile(`(?i)^(?:it|this|the (?:change|release|app|application)) (?:wasn't|isn't|hasn't been|was not|is not|has not been) (?:yet )?(?:deployed|published|shipped|released)\b`)
	unpublishedFilesRE   = regexp.MustCompile(`(?i)^(?:(?:[1-9][0-9]*|one|two|three|four|five|six|seven|eight|nine|ten|some|several) )?local (?:files|changes|edits) (?:remain|are|still remain) (?:unpublished|unshipped|unpushed)\b`)
	productionBehindRE   = regexp.MustCompile(`(?i)^production (?:is|remains) .+ commits? behind\b`)
	recoveredHistoryRE   = regexp.MustCompile(`(?i)\b(earlier|initially|previously)\b.*\b(succeeded|recovered|resolved|fixed|deployed)\b`)
	auditPromptRE        = regexp.MustCompile(`(?i)\b(read.only|audit|review only|inspect only|do not deploy|don't deploy|without deploy|no deployment)\b`)
	incompleteDeliveryRE = regexp.MustCompile(`(?i)^(?:(?:it|this|the (?:change|release|app|application)) (?:was|is|has|remains)|(?:i|we) (?:have|did|could|can)|deployment|production|deploy|release|shipping|delivery|not (?:yet )?deployed|undeployed)\b.*\b(not|never|cannot|can't|couldn't|wasn't|isn't|hasn't|haven't|didn't|failed|blocked|pending|undeployed)\b`)
	blockerClaimRE       = regexp.MustCompile(`(?i)^(?:(?:i(?:'m| am)|we(?:'re| are)|the (?:task|work|deployment)|deployment|delivery) (?:still |currently |now )?blocked\b|(?:remaining |unresolved )?blocker\s*:|(?:i|we) (?:cannot|can't|could not|couldn't|am unable to|are unable to) (?:continue|finish|complete|deploy|publish|ship)\b)`)
)

func terminalBlocker(message string) string {
	fenced := false
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced || strings.HasPrefix(line, ">") || strings.HasPrefix(line, `"`) || strings.HasPrefix(line, "“") {
			continue
		}
		line = strings.TrimLeft(line, "-*• ")
		line = strings.ReplaceAll(strings.ReplaceAll(line, "**", ""), "’", "'")
		lower := strings.ToLower(line)
		if recoveredHistoryRE.MatchString(line) {
			continue
		}
		if unpublishedFilesRE.MatchString(line) {
			return "ship"
		}
		if productionBehindRE.MatchString(line) {
			return "deploy"
		}
		if negativeDeliveryRE.MatchString(line) {
			return terminalDeliveryKind(line)
		}
		// Only direct status assertions count. Quoted examples and historical
		// failures are not current blocker claims; successful prose is never proof.
		if strings.HasPrefix(lower, "not deployed") || strings.HasPrefix(lower, "not yet deployed") || strings.HasPrefix(lower, "undeployed") {
			return "deploy"
		}
		if incompleteDeliveryRE.MatchString(line) && regexp.MustCompile(`(?i)\b(deploy(?:ed|ment)?|publish(?:ed)?|production|shipping|delivery|release)\b`).MatchString(line) {
			if !strings.Contains(lower, "wasn't deployed") && !strings.Contains(lower, "not deployed") && (strings.Contains(lower, "now deployed") || strings.Contains(lower, "successfully deployed")) {
				continue
			}
			return terminalDeliveryKind(line)
		}
		if blockerClaimRE.MatchString(line) {
			return "work"
		}
	}
	return ""
}

func terminalDeliveryKind(line string) string {
	if regexp.MustCompile(`(?i)\b(deploy(?:ed|ment)?|production|release(?:d)?)\b`).MatchString(line) {
		return "deploy"
	}
	return "ship"
}

func recordPromptContext(e event, w io.Writer) error {
	p, err := statePath(e)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadState(p, e.SessionID, e.TurnID)
	if err != nil {
		return err
	}
	s.CWD, s.AuditOnly = e.CWD, auditPromptRE.MatchString(e.Prompt)
	if err := save(p, s); err != nil {
		return err
	}
	return writeJSON(w, hookOutput{})
}

func refreshBlockers(s *state, e event) error {
	if e.CWD == "" {
		e.CWD = s.CWD
	}
	project := canonicalProjectKey(e.CWD)
	if s.NativeDeliveryProject != "" && s.NativeDeliveryProject != project {
		s.NativeDeliveryKnown = false
	}
	if s.LastFailedProject != "" && s.LastFailedProject != project {
		s.LastFailedOperation = ""
	}
	l, err := ledgerLoad(e)
	if err != nil {
		return err
	}
	snapshot := l.snapshot()
	s.OpenBlockers, s.UnsupportedBlockers = snapshot.Unresolved, snapshot.Unsupported
	s.VerifiedRecoveries = snapshot.Recoveries
	s.RecoveredThisTurn = snapshot.Recoveries > 0 && snapshot.LastRecoveryTurn == e.TurnID
	return nil
}

func recordOperationResult(s *state, e event, pending pendingCall, known, passed bool) error {
	if pending.OperationKey == "" {
		return nil
	}
	e.CWD = pending.WorkingDirectory
	if pending.OperationKind == "work" {
		if known && !passed {
			s.LastFailedOperation = pending.OperationKey
			s.LastFailedProject = canonicalProjectKey(e.CWD)
			l, err := ledgerLoad(e)
			if err != nil {
				return err
			}
			for _, obligation := range l.Obligations {
				if obligation.Kind == "work" && obligation.OperationKey == "work" && !obligation.Failed {
					// A failed reproduction identifies the previously unsupported
					// claim. Only a retry of this same command can prove recovery.
					if _, err := ledgerRecord(e, pending.OperationKey, "work", blockerStatusFailed); err != nil {
						return err
					}
					if _, err := ledgerRecord(e, "work", "work", blockerStatusSucceeded); err != nil {
						return err
					}
				}
			}
		}
		if !known || !passed {
			return nil
		}
		l, err := ledgerLoad(e)
		if err != nil {
			return err
		}
		if l.unresolvedCount() > 0 {
			if _, err := ledgerRecord(e, pending.OperationKey, "work", blockerStatusSucceeded); err != nil {
				return err
			}
			if s.LastFailedOperation == pending.OperationKey {
				if _, err := ledgerRecord(e, "work", "work", blockerStatusSucceeded); err != nil {
					return err
				}
			}
		}
		return nil
	}
	status := blockerStatusUnknown
	if known && !passed {
		status = blockerStatusFailed
	}
	if known && passed {
		if pending.Shipping && pending.OperationKind == "deploy" {
			return nil
		}
		// An explicit deploy can only resolve this project's delivery when its
		// immutable commit matches the checkout observed before execution.
		if pending.Deploying && !pending.DeployCommitMatch {
			return nil
		}
		status = blockerStatusSucceeded
	}
	_, err := ledgerRecord(e, pending.OperationKey, pending.OperationKind, status)
	return err
}

func recordTerminalBlocker(s *state, e event) error {
	if s.AuditOnly {
		return nil
	}
	kind := terminalBlocker(e.LastAssistantMsg)
	if kind == "" {
		return nil
	}
	key, status := kind, blockerStatusClaimed
	if kind == "work" && s.LastFailedOperation != "" {
		key, status = s.LastFailedOperation, blockerStatusFailed
	}
	_, err := ledgerRecord(e, key, kind, status)
	return err
}

func blockerReport(s state) string {
	if s.OpenBlockers > 0 {
		if s.UnsupportedBlockers > 0 {
			return "Blocker review: FAILED. An incomplete outcome has no verified blocker or recovery evidence. Passing unrelated checks does not complete the task."
		}
		return "Blocker review: FAILED. An observed failure remains unresolved. Local fixes and passing checks do not prove the affected operation succeeded."
	}
	if s.RecoveredThisTurn {
		return "Blocker review: INCREDIBLE WIN. An observed failure was overcome and the affected operation returned verified success."
	}
	return ""
}

func outcomeScore(s state) int {
	switch recordedOutcome(s) {
	case outcomeFailed:
		return 0
	case outcomeRecovered, outcomeVerified:
		return 100
	default:
		return 0
	}
}

func blockerOutput(e event, s state, message string) (hookOutput, error) {
	out := hookOutput{SystemMessage: message}
	if s.OpenBlockers == 0 {
		return out, nil
	}
	_, challenge, err := ledgerChallenge(e)
	if err != nil {
		return out, err
	}
	if challenge && !e.StopHookActive {
		out.Decision = "block"
		out.Reason = "one-shot-tally recorded FAILED: the requested outcome is still incomplete. Fact-check the blocker before stopping. Identify the attempted command, its actual result, and the specific missing access or external change. Diagnose and repair recoverable failures, then rerun the affected operation and verify its result. Passing unrelated tests or claiming success does not resolve this failure. Let native hooks own routine shipping. Do not repeat an upload whose result is unknown. If an external constraint is confirmed, report the evidence and leave the outcome FAILED; this challenge will not repeat for the same unresolved blocker."
	}
	return out, nil
}

// DeliveryResult is emitted by the native shipping process after the repository
// command returns. It records the actual handoff, not the agent's final prose.
func deliveryResult(e event, w io.Writer) error {
	if e.SessionID == "" || e.TurnID == "" || e.CWD == "" || e.DeliveryAttemptID == "" || !oneOf(e.DeliveryKind, "ship", "deploy") || !oneOf(e.DeliveryStatus, "failed", "succeeded") {
		return errors.New("invalid delivery receipt identity or status")
	}
	stamp, _, _ := strings.Cut(e.DeliveryAttemptID, "-")
	timestamp, stampErr := strconv.ParseInt(stamp, 10, 64)
	if stampErr != nil || timestamp <= 0 || !regexp.MustCompile(`^[1-9][0-9]*-[a-f0-9]{24}$`).MatchString(e.DeliveryAttemptID) {
		return errors.New("invalid native delivery attempt identifier")
	}
	known, passed := explicitResponseResult(e.ToolResponse)
	if !known || passed != (e.DeliveryStatus == "succeeded") {
		return errors.New("delivery receipt lacks a matching structured result")
	}
	if passed && !regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`).MatchString(e.DeliveryCommit) {
		return errors.New("successful delivery receipt lacks a full commit")
	}
	p, err := statePath(e)
	if err != nil {
		return err
	}
	unlock, err := acquireStateLock(p)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := loadState(p, e.SessionID, e.TurnID)
	if err != nil {
		return err
	}
	s.CWD = e.CWD
	if s.NativeDeliveryAttemptID == e.DeliveryAttemptID {
		return writeJSON(w, hookOutput{})
	}
	current := !passed || nativeRevisionCurrent(e.CWD, e.DeliveryCommit)
	status := e.DeliveryStatus
	if !current {
		status = blockerStatusUnknown
	}
	ledger, err := ledgerRecord(e, e.DeliveryKind, e.DeliveryKind, status)
	if err != nil {
		return err
	}
	if ledger.LastDeliveryID != e.DeliveryAttemptID {
		return writeJSON(w, hookOutput{SystemMessage: "Native delivery receipt ignored: this project already has a newer delivery result."})
	}
	s.NativeDeliveryKnown, s.NativeDeliverySucceeded, s.NativeDeliveryKind, s.NativeDeliveryAttemptID = true, passed, e.DeliveryKind, e.DeliveryAttemptID
	s.NativeDeliveryProject = canonicalProjectKey(e.CWD)
	s.ProductionAttempts++
	if passed {
		s.ProductionCompletions++
	}
	// An accepted native handoff supersedes earlier tool-level failure evidence
	// for delivery, while the durable ledger retains any other failed operation.
	if passed && current {
		s.LastCallResultKnown, s.LastCallSucceeded = true, true
		s.LastProductionResultKnown, s.LastProductionSucceeded = true, true
		s.LastProductionResultSequence = s.TotalCalls + 1
		s.ShipAttempts++
		s.ShipCompletions++
		s.LastShipResultKnown, s.LastShipSucceeded = true, true
		s.LastShipRevision, s.LastShipResultSequence = s.Revision, s.TotalCalls+1
		if e.DeliveryKind == "deploy" {
			s.DeployCompletions++
			s.DeployAttempts++
			s.LastDeployResultKnown, s.LastDeploySucceeded, s.LastDeployCommitMatch = true, true, true
			s.LastDeployRevision, s.LastDeployResultSequence = s.Revision, s.TotalCalls+1
		}
	}
	if err := refreshBlockers(&s, e); err != nil {
		return err
	}
	message := reportLine(s) + "\nNative delivery: " + e.DeliveryKind + " " + e.DeliveryStatus + "."
	if passed && !current {
		message += " The delivered commit does not establish a clean, current checkout; local delivery remains unverified."
	}
	out, err := blockerOutput(e, s, message)
	if err != nil {
		return err
	}
	if err := save(p, s); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(out)
}

func nativeRevisionCurrent(cwd, commit string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	head, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--verify", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(head)) != commit {
		return false
	}
	status, err := exec.CommandContext(ctx, "git", "-C", cwd, "status", "--porcelain", "--untracked-files=all").Output()
	return err == nil && strings.TrimSpace(string(status)) == ""
}
