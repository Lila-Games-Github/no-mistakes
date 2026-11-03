package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// The real review-fix incident failed at call 17 after the action had streamed,
// before its output pipe closed. No command from that failed action may run.
func TestBoundedWork_OutputDrainRetryAtCall17(t *testing.T) {
	defer withFastBackoff(t)()
	calls, commands := 0, 0
	var retryPrompt string
	var attempts []Attempt
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "repair and verify the assigned findings", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) }}, func(call RunOpts) (*Result, error) {
		calls++
		if calls <= 16 {
			return boundedTestResult(boundedAction{Checkpoint: fmt.Sprintf("repairs through %d preserved", calls), Command: fmt.Sprintf("edit %d", calls)}), nil
		}
		if calls == 17 {
			retryPrompt = call.Prompt
			return boundedTestResult(boundedAction{Command: "must not execute"}), fmt.Errorf("claude parse events: %w", exec.ErrWaitDelay)
		}
		if call.Prompt != retryPrompt || !strings.Contains(call.Prompt, "repairs through 16 preserved") || !strings.Contains(call.Prompt, "edit 16 persisted") {
			t.Fatal("I/O retry lost checkpoint or replayed the duty")
		}
		return boundedTestResult(boundedAction{Output: "all repairs verified"}), nil
	}, func(command string) (string, error) {
		commands++
		if command == "must not execute" {
			t.Fatal("executed an action from a failed native call")
		}
		return command + " persisted", nil
	})
	if err != nil || calls != 18 || commands != 16 || len(attempts) != 18 {
		t.Fatalf("calls=%d commands=%d attempts=%d err=%v", calls, commands, len(attempts), err)
	}
	if !errors.Is(attempts[16].Err, exec.ErrWaitDelay) || result.Usage.CacheReadTokens != 25000*18 || result.PeakContextTokens != 30000 {
		t.Fatalf("I/O failure lost cause or usage: result=%+v attempts=%+v", result, attempts)
	}
}

func TestBoundedWork_OutputDrainRetriesRespectLimits(t *testing.T) {
	defer withFastBackoff(t)()
	for _, mode := range []string{"budget", "per-call-budget", "cancel", "retry-limit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			opts := RunOpts{Prompt: "repair", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}
			if mode == "budget" {
				opts.MaxTotalInputTokens = 35000
			}
			if mode == "per-call-budget" {
				opts.MaxInputTokens = 20000
			}
			opts.OnLifecycle = func(event LifecycleEvent) {
				if mode == "cancel" && event.Phase == LifecyclePhaseRetry {
					cancel()
				}
			}
			result, err := runBoundedWork(ctx, opts, func(RunOpts) (*Result, error) {
				calls++
				return boundedTestResult(boundedAction{Command: "must not run"}), fmt.Errorf("claude parse events: %w", exec.ErrWaitDelay)
			}, func(string) (string, error) { t.Fatal("failed action ran a command"); return "", nil })
			wantCalls, wantErr := 1, ErrInputTokenBudget
			if mode == "cancel" {
				wantErr = context.Canceled
			}
			if mode == "retry-limit" {
				wantCalls, wantErr = claudeMaxRetries+1, exec.ErrWaitDelay
			}
			if !errors.Is(err, wantErr) || calls != wantCalls || result.Usage.CacheReadTokens != 25000*calls {
				t.Fatalf("calls=%d result=%+v err=%v want=%v", calls, result, err, wantErr)
			}
		})
	}
}

func TestBoundedWork_TransientRetryPreservesEditsAndCountsEveryCall(t *testing.T) {
	defer withFastBackoff(t)()
	calls, commands := 0, 0
	var attempts []Attempt
	var retriedPrompt string
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "fix the assigned source", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) }}, func(call RunOpts) (*Result, error) {
		calls++
		if call.Session != nil || len(call.Prompt)+boundedFramingReserve > call.MaxInputTokens {
			t.Fatal("retry imported a transcript or exceeded the preflight budget")
		}
		switch calls {
		case 1:
			return boundedTestResult(boundedAction{Checkpoint: "one authorized edit is next", Command: "edit once"}), nil
		case 2:
			retriedPrompt = call.Prompt
			return boundedTestResult(boundedAction{}), errors.New("503 overloaded_error")
		default:
			if call.Prompt != retriedPrompt || !strings.Contains(call.Prompt, "edit persisted") {
				t.Fatal("retry lost the checkpoint or last observation")
			}
			return boundedTestResult(boundedAction{Checkpoint: "repair complete", Output: "fixed"}), nil
		}
	}, func(string) (string, error) { commands++; return "edit persisted", nil })
	if err != nil || calls != 3 || commands != 1 || len(attempts) != 3 {
		t.Fatalf("calls=%d commands=%d attempts=%d err=%v", calls, commands, len(attempts), err)
	}
	if attempts[1].Err == nil || result.Usage.CacheReadTokens != 75000 || result.PeakContextTokens != 30000 {
		t.Fatalf("failed retry was not counted: result=%+v attempts=%+v", result, attempts)
	}
}

func TestBoundedWork_TransientRetriesRespectRunBudgetAndCancellation(t *testing.T) {
	defer withFastBackoff(t)()
	for _, cancelDuringBackoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "cancel"}[cancelDuringBackoff], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			opts := RunOpts{Prompt: "fix", MaxInputTokens: 150000, MaxTotalInputTokens: 35000}
			if cancelDuringBackoff {
				opts.OnLifecycle = func(event LifecycleEvent) {
					if event.Phase == LifecyclePhaseRetry {
						cancel()
					}
				}
			}
			result, err := runBoundedWork(ctx, opts, func(RunOpts) (*Result, error) {
				calls++
				return boundedTestResult(boundedAction{}), errors.New("503 overloaded_error")
			}, func(string) (string, error) { t.Fatal("retry executed a command"); return "", nil })
			want := ErrInputTokenBudget
			if cancelDuringBackoff {
				want = context.Canceled
			}
			if !errors.Is(err, want) || calls != 1 || result.Usage.CacheReadTokens != 25000 {
				t.Fatalf("calls=%d result=%+v err=%v, want %v", calls, result, err, want)
			}
		})
	}
}

func TestBoundedWork_TransientRetryLimit(t *testing.T) {
	defer withFastBackoff(t)()
	calls := 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(RunOpts) (*Result, error) {
		calls++
		return boundedTestResult(boundedAction{}), errors.New("503 overloaded_error")
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if err == nil || calls != claudeMaxRetries+1 || result.Usage.CacheReadTokens != 25000*calls {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestBoundedWork_TransientFailureCannotBypassPerCallBudget(t *testing.T) {
	calls := 0
	_, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", MaxInputTokens: 20000, MaxTotalInputTokens: 1500000}, func(RunOpts) (*Result, error) {
		calls++
		return boundedTestResult(boundedAction{}), errors.New("503 overloaded_error")
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if !errors.Is(err, ErrInputTokenBudget) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestBoundedWork_RetryUsesRemainingRunBudget(t *testing.T) {
	defer withFastBackoff(t)()
	calls := 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", MaxInputTokens: 150000, MaxTotalInputTokens: 85000}, func(call RunOpts) (*Result, error) {
		calls++
		wantLimit := 85000 - (calls-1)*30000
		if call.MaxInputTokens != wantLimit {
			t.Fatalf("retry limit=%d want %d", call.MaxInputTokens, wantLimit)
		}
		result := boundedTestResult(boundedAction{Output: "complete"})
		if calls < 3 {
			return result, errors.New("503 overloaded_error")
		}
		result.Usage.CacheReadTokens -= 5000
		return result, nil
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if err != nil || calls != 3 || result.Usage.InputTokens+result.Usage.CacheReadTokens+result.Usage.CacheCreationTokens != 85000 {
		t.Fatalf("calls=%d usage=%+v err=%v", calls, result.Usage, err)
	}
}

var boundedRecoverySchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"risk_scope":{"type":"string"}},"required":["summary","risk_scope"]}`)

func TestBoundedWork_FinalOutputUsesMechanicalRepair(t *testing.T) {
	calls := 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", JSONSchema: boundedRecoverySchema, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(RunOpts) (*Result, error) {
		calls++
		return boundedTestResult(boundedAction{Output: "{\"summary\":\"source\nreviewed\",\"risk_scope\":\"source\",}"}), nil
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if err != nil || calls != 1 || !result.StructuredOutputRepaired || result.StructuredOutputReasks != 0 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
	var output map[string]string
	if err := json.Unmarshal(result.Output, &output); err != nil || output["summary"] != "source\nreviewed" {
		t.Fatalf("mechanical repair changed content: %s err=%v", result.Output, err)
	}
}

func TestBoundedWork_FinalCorrectionPreservesCompleteResponseAndEdits(t *testing.T) {
	// Longer than a single prompt: the correction must retain the whole rejected
	// response through exact pages, including findings beyond a short snippet.
	summary := strings.Repeat("source:77 substantiated finding; ", 1700) + "TAIL FINDING source:1999"
	rejected, _ := json.Marshal(map[string]string{"summary": summary})
	corrected, _ := json.Marshal(map[string]string{"summary": summary, "risk_scope": "changed source"})
	calls, commands, seenPages := 0, 0, 0
	var correctionPages []string
	var attempts []Attempt
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "fix all assigned findings", JSONSchema: boundedRecoverySchema, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) }}, func(call RunOpts) (*Result, error) {
		calls++
		if len(call.Prompt) > boundedPromptBytes || call.Session != nil {
			t.Fatal("correction is not bounded and cold")
		}
		if calls == 1 {
			return boundedTestResult(boundedAction{Checkpoint: "authorized edit next", Command: "edit once"}), nil
		}
		if calls == 2 {
			return boundedTestResult(boundedAction{Checkpoint: "all source checked; authorized edit verified; every finding retained", Output: string(rejected)}), nil
		}
		if !call.StructuredOutputReask {
			t.Fatal("correction not identified in instrumentation")
		}
		if correctionPages == nil {
			var soErr *StructuredOutputError
			if !errors.As(attempts[1].Err, &soErr) {
				t.Fatalf("final failure lost typed validation error: %v", attempts[1].Err)
			}
			correctionPages, _ = boundedTaskPages(structuredOutputReaskPrompt(soErr, boundedRecoverySchema, false))
		}
		if !strings.Contains(call.Prompt, correctionPages[seenPages]) {
			t.Fatal("correction dropped or truncated rejected response")
		}
		seenPages++
		if seenPages < len(correctionPages) {
			return boundedTestResult(boundedAction{Checkpoint: "all prior findings retained; formatting correction only"}), nil
		}
		return boundedTestResult(boundedAction{Checkpoint: "complete findings preserved", Output: string(corrected)}), nil
	}, func(string) (string, error) { commands++; return "edit persisted and focused verification passed", nil })
	if err != nil || commands != 1 || seenPages != len(correctionPages) || result.StructuredOutputReasks != 1 || result.Usage.CacheReadTokens != 25000*calls {
		t.Fatalf("calls=%d commands=%d pages=%d err=%v", calls, commands, seenPages, err)
	}
	var output map[string]string
	if err := json.Unmarshal(result.Output, &output); err != nil || output["summary"] != summary {
		t.Fatalf("correction lost findings: err=%v", err)
	}
}

func TestBoundedWork_FinalCorrectionSharesRunBudget(t *testing.T) {
	calls := 0
	result, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", JSONSchema: boundedRecoverySchema, MaxInputTokens: 150000, MaxTotalInputTokens: 35000}, func(RunOpts) (*Result, error) {
		calls++
		return boundedTestResult(boundedAction{Output: `{"summary":"source reviewed"}`}), nil
	}, func(string) (string, error) { t.Fatal("unexpected command"); return "", nil })
	if !errors.Is(err, ErrInputTokenBudget) || calls != 1 || result.Usage.CacheReadTokens != 25000 {
		t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
	}
}

func TestBoundedWork_FinalCorrectionCannotRunCommandsOrReaskTwice(t *testing.T) {
	for _, commandDuringCorrection := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-again", true: "command"}[commandDuringCorrection], func(t *testing.T) {
			calls := 0
			_, err := runBoundedWork(context.Background(), RunOpts{Prompt: "review", JSONSchema: boundedRecoverySchema, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000}, func(call RunOpts) (*Result, error) {
				calls++
				if calls > 2 {
					t.Fatal("more than one final correction")
				}
				if calls == 2 && commandDuringCorrection {
					return boundedTestResult(boundedAction{Command: "must not run"}), nil
				}
				return boundedTestResult(boundedAction{Output: `{"summary":"source reviewed; risk is changed source"}`}), nil
			}, func(string) (string, error) { t.Fatal("command executed during formatting correction"); return "", nil })
			if err == nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
