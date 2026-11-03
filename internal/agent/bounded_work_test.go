package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func boundedTestResult(action boundedAction) *Result {
	data, _ := json.Marshal(action)
	return &Result{Text: string(data), UsageReported: true, CacheCreationReported: true,
		Usage: TokenUsage{InputTokens: 2, CacheReadTokens: 25000, CacheCreationTokens: 4998, Reported: true, CacheCreationReported: true}}
}

func TestBoundedWork_LargeObservationsDoNotAccumulate(t *testing.T) {
	var capture boundedCapture
	for i := 0; i < 2048; i++ {
		_, _ = capture.Write([]byte(strings.Repeat("x", 1024)))
	}
	calls, commands, attempts := 0, 0, 0
	opts := RunOpts{Prompt: "Review all source and retain every finding", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		Session: &SessionRef{ID: "old-fixer"}, JSONSchema: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}},"required":["findings"]}`),
		OnAttempt: func(attempt Attempt) {
			attempts++
			if attempt.Result == nil || attempt.Result.Usage.CacheReadTokens != 25000 {
				t.Fatal("usage lost from attempt")
			}
		}}
	result, err := runBoundedWork(context.Background(), opts, func(call RunOpts) (*Result, error) {
		calls++
		if call.Session != nil || len(call.JSONSchema) != 0 || len(call.Prompt) > boundedPromptBytes {
			t.Fatal("native call was not cold and bounded")
		}
		if calls == 1 {
			return boundedTestResult(boundedAction{Checkpoint: "early finding at source:77; rest of scope outstanding", Command: "read source"}), nil
		}
		if !strings.Contains(call.Prompt, "early finding at source:77") || !strings.Contains(call.Prompt, "output truncated") || len(call.Prompt) > 35000 {
			t.Fatal("checkpoint or bounded output missing")
		}
		return boundedTestResult(boundedAction{Checkpoint: "all scope checked", Output: `{"findings":["early finding at source:77"]}`}), nil
	}, func(string) (string, error) { commands++; return capture.String(), nil })
	if err != nil || calls != 2 || commands != 1 || attempts != 2 {
		t.Fatalf("calls=%d commands=%d attempts=%d err=%v", calls, commands, attempts, err)
	}
	if result.Usage.InputTokens+result.Usage.CacheReadTokens+result.Usage.CacheCreationTokens != 60000 || result.PeakContextTokens != 30000 {
		t.Fatalf("usage lost: %+v", result)
	}
}

func TestBoundedWork_LongTaskIsFullyIngestedAndCanBeReread(t *testing.T) {
	task := strings.Repeat("αβγ durable invariant\n", 2000)
	pages, err := boundedTaskPages(task)
	if err != nil || strings.Join(pages, "") != task {
		t.Fatalf("task pages lose data: %v", err)
	}
	calls, commands := 0, 0
	_, err = runBoundedWork(context.Background(), RunOpts{Prompt: task, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(call RunOpts) (*Result, error) {
		calls++
		if calls < len(pages) {
			if !strings.Contains(call.Prompt, pages[calls-1]) || !strings.Contains(call.Prompt, "in progress: true") {
				t.Fatal("missing task page")
			}
			return boundedTestResult(boundedAction{Checkpoint: "constraints retained"}), nil
		}
		if calls == len(pages) {
			return boundedTestResult(boundedAction{Checkpoint: "all task pages ingested", ReadPage: 1}), nil
		}
		if !strings.Contains(call.Prompt, pages[0]) {
			t.Fatal("exact earlier page unavailable")
		}
		return boundedTestResult(boundedAction{Checkpoint: "full coverage", Output: "complete"}), nil
	}, func(string) (string, error) { commands++; return "", nil })
	if err != nil || calls != len(pages)+1 || commands != 0 {
		t.Fatalf("calls=%d commands=%d err=%v", calls, commands, err)
	}
}

func TestBoundedWork_RejectsPartialCompletionAndInvalidFinalResult(t *testing.T) {
	for _, task := range []string{"small task", strings.Repeat("large task ", 3000)} {
		_, err := runBoundedWork(context.Background(), RunOpts{Prompt: task, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
			JSONSchema: json.RawMessage(`{"type":"object","properties":{"findings":{"type":"array"}},"required":["findings"]}`)}, func(RunOpts) (*Result, error) {
			return boundedTestResult(boundedAction{Output: `{"summary":"only partially done"}`}), nil
		}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
		if err == nil {
			t.Fatal("partial work accepted")
		}
	}
}

func TestBoundedWork_CancellationDoesNotLaunchAnotherCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := runBoundedWork(ctx, RunOpts{Prompt: "fix", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(RunOpts) (*Result, error) {
		cancel()
		return boundedTestResult(boundedAction{Command: "must not run"}), nil
	}, func(string) (string, error) { t.Fatal("command started after cancellation"); return "", nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestBoundedWork_StopsNoProgressWithoutAcceptingPartialWork(t *testing.T) {
	calls := 0
	_, err := runBoundedWork(context.Background(), RunOpts{Prompt: "fix", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(RunOpts) (*Result, error) {
		calls++
		return boundedTestResult(boundedAction{Checkpoint: "still investigating", Command: "read same file"}), nil
	}, func(string) (string, error) { return "same contents", nil })
	if err == nil || !strings.Contains(err.Error(), "no progress") || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestBoundedWork_ReservesRunBudgetBeforeStartingAnotherCall(t *testing.T) {
	calls := 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "fix", MaxInputTokens: 150000, MaxTotalInputTokens: 20000}, func(RunOpts) (*Result, error) {
		calls++
		result := boundedTestResult(boundedAction{Checkpoint: "partial edit preserved", Command: "repair one part"})
		result.Usage = TokenUsage{InputTokens: 10000, Reported: true}
		return result, nil
	}, func(string) (string, error) { return "repair preserved", nil })
	if !errors.Is(err, ErrInputTokenBudget) || calls != 1 || result.Usage.InputTokens != 10000 {
		t.Fatalf("calls=%d usage=%+v err=%v", calls, result.Usage, err)
	}
}

func TestBoundedWork_CorrectiveCallKeepsUsageWithoutReplayingTools(t *testing.T) {
	calls, attempts, commands := 0, 0, 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(Attempt) { attempts++ }}, func(call RunOpts) (*Result, error) {
		calls++
		if calls == 1 {
			result := boundedTestResult(boundedAction{})
			result.Text = "malformed action JSON"
			return result, nil
		}
		if !strings.Contains(call.Prompt, "Your action JSON was rejected") {
			t.Fatal("missing correction context")
		}
		return boundedTestResult(boundedAction{Output: "complete"}), nil
	}, func(string) (string, error) { commands++; return "", nil })
	if err != nil || calls != 2 || attempts != 2 || commands != 0 || result.Usage.CacheReadTokens != 50000 {
		t.Fatalf("calls=%d attempts=%d commands=%d usage=%+v err=%v", calls, attempts, commands, result.Usage, err)
	}
}

func TestClaudeAgent_BoundedModeRejectsNativeResumeOverride(t *testing.T) {
	a := &claudeAgent{bin: "must-not-be-started", extraArgs: []string{"--resume", "foreign-review-session"}}
	_, err := a.Run(context.Background(), RunOpts{Prompt: "review", BoundedWork: true, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000})
	if err == nil || !strings.Contains(err.Error(), "context-expanding agent_args_override flag --resume") {
		t.Fatalf("unsafe override = %v", err)
	}
}
