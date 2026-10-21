package main

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

const longWaitMillis int64 = 10_000

func isWaitObservation(e event, command string) bool {
	tool := normalizedToolName(e.ToolName)
	if oneOf(tool, "wait", "waitagent", "collaborationwaitagent", "sleep") {
		return true
	}
	if tool == "writestdin" {
		var input map[string]json.RawMessage
		if json.Unmarshal(e.ToolInput, &input) != nil || input == nil {
			return false
		}
		if raw, present := input["chars"]; present {
			var chars string
			if json.Unmarshal(raw, &chars) != nil || string(raw) == "null" || chars != "" {
				return false
			}
		}
		e.ToolName = "write_stdin"
		return passiveWait(e, command)
	}
	// Avoid passiveWait's broad tool-name substring match: an ordinary tool
	// whose name happens to contain "wait" is not an observed wait.
	return isCommandTool(e.ToolName) && passiveWait(e, command)
}

func recordWaitFinish(s *state, p pendingCall, raw json.RawMessage, now time.Time) {
	if !p.Wait || p.WaitStartedAt.IsZero() {
		return
	}
	millis, ok := observedWaitMillis(raw)
	if !ok {
		millis = now.Sub(p.WaitStartedAt).Milliseconds()
		if millis < 0 {
			millis = 0
		}
	}
	s.TotalWaitMillis = addWaitMillis(s.TotalWaitMillis, millis)
	if millis > longWaitMillis {
		if s.LongWaits < int(^uint(0)>>1) {
			s.LongWaits++
		}
		s.ExcessWaitMillis = addWaitMillis(s.ExcessWaitMillis, millis-longWaitMillis)
	}
}

// Only native response-envelope timings are evidence. Text output, nested
// objects, requested timeouts, numeric strings and invalid numbers are not.
func observedWaitMillis(raw json.RawMessage) (int64, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil {
		return 0, false
	}
	for _, field := range []struct {
		name  string
		scale float64
	}{{"wall_time_seconds", 1000}, {"duration_seconds", 1000}, {"duration_ms", 1}} {
		value, present := envelope[field.name]
		if !present || string(value) == "null" {
			continue
		}
		var number float64
		if json.Unmarshal(value, &number) != nil || number < 0 || math.IsNaN(number) || math.IsInf(number, 0) {
			continue
		}
		millis := number * field.scale
		if math.IsInf(millis, 0) || millis >= float64(math.MaxInt64) {
			return math.MaxInt64, true
		}
		return int64(millis), true
	}
	return 0, false
}

func addWaitMillis(current, extra int64) int64 {
	if current < 0 {
		current = 0
	}
	if extra >= math.MaxInt64-current {
		return math.MaxInt64
	}
	return current + extra
}

func waitPenalty(s state) int {
	passive := 0
	if s.PassiveWaits >= 4 {
		passive = 25
	} else if s.PassiveWaits > 0 {
		passive = s.PassiveWaits * 7
	}
	frequency := 0
	if s.WaitCalls >= 16 {
		frequency = 70
	} else if s.WaitCalls > 2 {
		frequency = (s.WaitCalls - 2) * 5
	}
	excess := int64(0)
	if s.ExcessWaitMillis > 0 {
		excess = s.ExcessWaitMillis / longWaitMillis
		if s.ExcessWaitMillis%longWaitMillis != 0 {
			excess++
		}
	}
	if excess >= 14 {
		return 70
	}
	combined := minInt(70, frequency+int(excess)*5)
	if passive > combined {
		return passive
	}
	return combined
}

func waitFeedback(s *state) string {
	if s.WaitFeedbackLevel == 0 && (s.WaitCalls >= 3 || s.LongWaits > 0) {
		s.WaitFeedbackLevel = 1
		if waitPenalty(*s) >= 25 {
			s.WaitFeedbackLevel = 2
		}
	} else if s.WaitFeedbackLevel == 1 && waitPenalty(*s) >= 25 {
		s.WaitFeedbackLevel = 2
	} else {
		return ""
	}
	return fmt.Sprintf("%s Park the existing operation and add one TODO check-in with its job/handle, status command, next check trigger and expected completion. Continue independent work. If progress stops, report observed evidence, the exact dependency and the step needed to unblock it. Keep it open; do not mark it complete or retry an external write with an unknown outcome. This is advisory; tool use continues.", waitReport(*s))
}

func waitReport(s state) string {
	if s.WaitCalls <= 0 {
		return ""
	}
	return fmt.Sprintf("Waits: %d calls; %d over 10s; %.1fs elapsed; %d-point activity penalty.", s.WaitCalls, s.LongWaits, float64(s.TotalWaitMillis)/1000, waitPenalty(s))
}
