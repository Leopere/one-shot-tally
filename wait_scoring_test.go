package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestWaitObservationRecognizesContinuationAndPollingTools(t *testing.T) {
	for _, test := range []struct {
		name, tool, input, command string
		want                       bool
	}{
		{"wait", "functions.wait", `{}`, "", true},
		{"agent wait", "collaboration.wait_agent", `{}`, "", true},
		{"flattened agent wait", "collaborationwaitagent", `{}`, "", true},
		{"sleep", "clock.sleep", `{}`, "", true},
		{"empty stdin", "functions.write_stdin", `{"session_id":12,"chars":""}`, "", true},
		{"omitted stdin", "functions.write_stdin", `{"session_id":12}`, "", true},
		{"normalized stdin", "writeStdin", `{"chars":""}`, "", true},
		{"missing stdin input", "functions.write_stdin", `null`, "", false},
		{"active stdin", "functions.write_stdin", `{"chars":"yes\n"}`, "", false},
		{"whitespace stdin", "functions.write_stdin", `{"chars":" "}`, "", false},
		{"interrupt stdin", "functions.write_stdin", `{"chars":"\u0003"}`, "", false},
		{"invalid stdin", "functions.write_stdin", `{"chars":false}`, "", false},
		{"null stdin", "functions.write_stdin", `{"chars":null}`, "", false},
		{"shell sleep", "functions.exec_command", `{}`, "sleep 60", true},
		{"ordinary work", "functions.exec_command", `{}`, "go test ./...", false},
		{"wait named work", "wait_for_build_configuration", `{}`, "", false},
		{"wait mentioned in output", "functions.exec_command", `{}`, "printf 'sleep 60'", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := isWaitObservation(event{ToolName: test.tool, ToolInput: json.RawMessage(test.input)}, test.command)
			if got != test.want {
				t.Fatalf("isWaitObservation() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRecordWaitFinishUsesObservedDurationsOnly(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	for _, test := range []struct {
		name, response string
		want           int64
	}{
		{"wall seconds", `{"wall_time_seconds":15.25}`, 15_250},
		{"duration seconds", `{"duration_seconds":0.5}`, 500},
		{"milliseconds", `{"duration_ms":10001}`, 10_001},
		{"real zero", `{"wall_time_seconds":0}`, 0},
		{"native precedence", `{"wall_time_seconds":1,"duration_ms":90000}`, 1000},
		{"invalid first field", `{"wall_time_seconds":"90","duration_ms":321}`, 321},
		{"negative field", `{"wall_time_seconds":-50}`, 250},
		{"string field", `{"duration_ms":"90000"}`, 250},
		{"null field", `{"wall_time_seconds":null}`, 250},
		{"forged stdout", `{"output":"wall_time_seconds: 500"}`, 250},
		{"nested timing", `{"output":{"wall_time_seconds":500},"result":{"duration_ms":90000}}`, 250},
		{"array timing", `[{"wall_time_seconds":500}]`, 250},
		{"requested timeout", `{"timeout_ms":90000,"yield_time_ms":60000,"duration":999}`, 250},
		{"invalid response", `not JSON`, 250},
		{"empty response", ``, 250},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := state{}
			recordWaitFinish(&s, pendingCall{Wait: true, WaitStartedAt: started}, json.RawMessage(test.response), started.Add(250*time.Millisecond))
			wantLong, wantExcess := 0, int64(0)
			if test.want > longWaitMillis {
				wantLong, wantExcess = 1, test.want-longWaitMillis
			}
			if s.TotalWaitMillis != test.want || s.LongWaits != wantLong || s.ExcessWaitMillis != wantExcess {
				t.Fatalf("recorded wait = %dms, %d long, %dms excess; want %dms, %d long, %dms excess", s.TotalWaitMillis, s.LongWaits, s.ExcessWaitMillis, test.want, wantLong, wantExcess)
			}
		})
	}
}

func TestRecordWaitFinishIgnoresUnobservedAndClockReversal(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	for _, p := range []pendingCall{{}, {Wait: true}, {WaitStartedAt: started}} {
		s := state{}
		recordWaitFinish(&s, p, json.RawMessage(`{"duration_seconds":90}`), started.Add(time.Minute))
		if s.TotalWaitMillis != 0 || s.LongWaits != 0 || s.ExcessWaitMillis != 0 {
			t.Fatalf("unobserved wait counted: %#v", s)
		}
	}
	s := state{}
	recordWaitFinish(&s, pendingCall{Wait: true, WaitStartedAt: started}, nil, started.Add(-time.Second))
	if s.TotalWaitMillis != 0 {
		t.Fatalf("clock reversal counted: %d", s.TotalWaitMillis)
	}
}

func TestWaitPenaltyBalancesFrequencyAndDuration(t *testing.T) {
	for _, test := range []struct {
		name string
		s    state
		want int
	}{
		{"none", state{}, 0},
		{"two short continuations", state{WaitCalls: 2}, 0},
		{"third short continuation", state{WaitCalls: 3}, 5},
		{"frequent continuations", state{WaitCalls: 7}, 25},
		{"long first wait", state{WaitCalls: 1, ExcessWaitMillis: 1}, 5},
		{"long duration rounded up", state{WaitCalls: 1, ExcessWaitMillis: 10_001}, 10},
		{"frequency plus duration", state{WaitCalls: 4, ExcessWaitMillis: 20_000}, 20},
		{"legacy passive penalty", state{PassiveWaits: 3, WaitCalls: 3}, 21},
		{"passive cap", state{PassiveWaits: 4}, 25},
		{"combined cap", state{WaitCalls: 15, ExcessWaitMillis: 50_000}, 70},
		{"saturated counters", state{PassiveWaits: int(^uint(0) >> 1), WaitCalls: int(^uint(0) >> 1), ExcessWaitMillis: math.MaxInt64}, 70},
		{"invalid negative counters", state{PassiveWaits: -1, WaitCalls: -1, ExcessWaitMillis: -1}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := waitPenalty(test.s); got != test.want {
				t.Fatalf("waitPenalty() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRecordWaitFinishAccumulatesAndSaturates(t *testing.T) {
	started := time.Unix(1_700_000_000, 0)
	p := pendingCall{Wait: true, WaitStartedAt: started}
	s := state{}
	for _, ms := range []string{"10000", "10001", "30000"} {
		recordWaitFinish(&s, p, json.RawMessage(`{"duration_ms":`+ms+`}`), started)
	}
	if s.TotalWaitMillis != 50_001 || s.LongWaits != 2 || s.ExcessWaitMillis != 20_001 {
		t.Fatalf("unexpected accumulation: %#v", s)
	}
	s = state{TotalWaitMillis: math.MaxInt64 - 1, ExcessWaitMillis: math.MaxInt64 - 1, LongWaits: int(^uint(0) >> 1)}
	recordWaitFinish(&s, p, json.RawMessage(`{"duration_seconds":1e300}`), started)
	if s.TotalWaitMillis != math.MaxInt64 || s.ExcessWaitMillis != math.MaxInt64 || s.LongWaits != int(^uint(0)>>1) {
		t.Fatalf("duration counters overflowed: %#v", s)
	}
}

func TestWaitFeedbackIsActionableAdvisoryAndBounded(t *testing.T) {
	s := state{WaitCalls: 2}
	if got := waitFeedback(&s); got != "" || s.WaitFeedbackLevel != 0 {
		t.Fatalf("premature feedback: %q, level %d", got, s.WaitFeedbackLevel)
	}
	s.WaitCalls = 3
	first := waitFeedback(&s)
	for _, fragment := range []string{"TODO check-in", "job/handle", "status command", "next check trigger", "expected completion", "independent work", "observed evidence", "exact dependency", "unblock", "unknown outcome", "advisory"} {
		if !strings.Contains(first, fragment) {
			t.Fatalf("feedback missing %q: %q", fragment, first)
		}
	}
	if got := waitFeedback(&s); got != "" || s.WaitFeedbackLevel != 1 {
		t.Fatalf("repeated initial feedback: %q, level %d", got, s.WaitFeedbackLevel)
	}
	s.WaitCalls = 7
	if got := waitFeedback(&s); got == "" || s.WaitFeedbackLevel != 2 {
		t.Fatalf("missing escalation feedback: %q, level %d", got, s.WaitFeedbackLevel)
	}
	for i := 0; i < 20; i++ {
		s.WaitCalls++
		if got := waitFeedback(&s); got != "" {
			t.Fatalf("feedback exceeds two notices: %q", got)
		}
	}
	long := state{WaitCalls: 1, LongWaits: 1, TotalWaitMillis: 15_000, ExcessWaitMillis: 5000}
	if got := waitFeedback(&long); got == "" || long.WaitFeedbackLevel != 1 {
		t.Fatalf("missing first long wait feedback: %q", got)
	}
	severe := state{WaitCalls: 1, LongWaits: 1, ExcessWaitMillis: 100_000}
	if waitFeedback(&severe) == "" || severe.WaitFeedbackLevel != 2 || waitFeedback(&severe) != "" {
		t.Fatal("one severe wait caused repeated advisories without new waiting")
	}
}

func TestWaitReportOnlyIncludesObservedWaits(t *testing.T) {
	if got := waitReport(state{PassiveWaits: 2}); got != "" {
		t.Fatalf("unexpected report with no observed waits: %q", got)
	}
	report := waitReport(state{WaitCalls: 3, LongWaits: 1, TotalWaitMillis: 12_345, ExcessWaitMillis: 2345})
	for _, fragment := range []string{"3 calls", "1 over 10s", "12.3s elapsed", "10-point activity penalty"} {
		if !strings.Contains(report, fragment) {
			t.Fatalf("report missing %q: %q", fragment, report)
		}
	}
}
