package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func newClaudeBoundedHelperAgent(t *testing.T, mode string) (*claudeAgent, string) {
	t.Helper()
	t.Setenv("NM_CLAUDE_BOUNDED_HELPER", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &claudeAgent{bin: exe, extraArgs: []string{"-test.run=^TestClaudeBoundedHelper$", "--"}}, t.TempDir()
}

func TestClaudeBoundedHelper(t *testing.T) {
	mode := os.Getenv("NM_CLAUDE_BOUNDED_HELPER")
	if mode == "" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	data, _ := os.ReadFile("calls")
	n, _ := strconv.Atoi(string(data))
	n++
	if err := os.WriteFile("calls", []byte(strconv.Itoa(n)), 0o644); err != nil {
		os.Exit(2)
	}
	action, _ := json.Marshal(boundedAction{Checkpoint: "all assigned source reviewed", Output: "complete"})
	assistant, _ := json.Marshal(map[string]any{"type": "assistant", "session_id": "failure-session", "message": map[string]any{
		"id": "model-call", "model": "failure-model", "usage": map[string]int{"input_tokens": 2, "cache_read_input_tokens": 25000, "cache_creation_input_tokens": 4998},
		"content": []any{map[string]string{"type": "text", "text": string(action)}}}})
	fmt.Println(string(assistant))
	switch mode {
	case "native-error":
		fmt.Println(`{"type":"result","subtype":"error_max_turns","is_error":true}`)
	case "exit-error":
		fmt.Fprintln(os.Stderr, "native process failed")
		os.Exit(1)
	case "missing-result":
		return
	case "transient":
		if n == 1 {
			fmt.Fprintln(os.Stderr, "503 overloaded_error")
			os.Exit(1)
		}
		fmt.Println(`{"type":"result","subtype":"success"}`)
	default:
		fmt.Println(`{"type":"result","subtype":"success"}`)
	}
}

func TestClaudeAgent_BoundedFailuresPreserveUsageAndMetrics(t *testing.T) {
	for _, mode := range []string{"native-error", "exit-error", "missing-result", "parse-cancel", "budget"} {
		t.Run(mode, func(t *testing.T) {
			a, dir := newClaudeBoundedHelperAgent(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var attempts []Attempt
			opts := RunOpts{Prompt: "review assigned source", CWD: dir, BoundedWork: true, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
				OnAttempt: func(a Attempt) { attempts = append(attempts, a) }}
			if mode == "parse-cancel" {
				opts.OnChunk = func(string) { cancel() }
			}
			if mode == "budget" {
				opts.MaxInputTokens = 20000
			}
			result, err := a.Run(ctx, opts)
			if err == nil || len(attempts) != 1 || attempts[0].Err == nil {
				t.Fatalf("failure not surfaced: attempts=%d err=%v", len(attempts), err)
			}
			if mode == "parse-cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("parse failure lost cancellation: %v", err)
			}
			if mode == "budget" && !errors.Is(err, ErrInputTokenBudget) {
				t.Fatalf("native budget failure lost its cause: %v", err)
			}
			for _, res := range []*Result{result, attempts[0].Result} {
				if res == nil || !res.UsageReported || !res.CacheCreationReported || res.Usage.InputTokens != 2 || res.Usage.CacheReadTokens != 25000 || res.Usage.CacheCreationTokens != 4998 || res.Model != "failure-model" || res.ModelProvider != "anthropic" {
					t.Fatalf("partial consumption or identity disappeared on failure: %+v", res)
				}
			}
			if attempts[0].Result.Metrics == nil || attempts[0].Result.Metrics.ModelRoundtrips != 1 || attempts[0].Result.SessionID != "failure-session" {
				t.Fatalf("failure metrics or native identity lost: %+v", attempts[0].Result)
			}
		})
	}
}

type claudeBrokenStream struct{ err error }

func (r claudeBrokenStream) Read([]byte) (int, error) { return 0, r.err }

func TestParseClaudeEvents_ReadFailureRetainsStreamSnapshot(t *testing.T) {
	streamErr := errors.New("broken event stream")
	reader := io.MultiReader(strings.NewReader(`{"type":"assistant","session_id":"partial-session","message":{"id":"partial","model":"partial-model","usage":{"input_tokens":30},"content":[{"type":"text","text":"partial work"}]}}`+"\n"), claudeBrokenStream{streamErr})
	var usage TokenUsage
	var metrics InvocationMetrics
	snapshot := &claudeResult{}
	err := parseClaudeEventsDetailed(context.Background(), reader, nil, &usage, &snapshot, &metrics, 150000)
	if !errors.Is(err, streamErr) || usage.InputTokens != 30 || !usage.Reported || metrics.ModelRoundtrips != 1 || snapshot.sessionID != "partial-session" || snapshot.model != "partial-model" || snapshot.text != "partial work" || len(snapshot.rawEvent) != 0 {
		t.Fatalf("read failure lost partial stream or invented completion: snapshot=%+v usage=%+v metrics=%+v err=%v", snapshot, usage, metrics, err)
	}
}

func TestClaudeAgent_BoundedTransientFailureIsRetriedWithUsage(t *testing.T) {
	defer withFastBackoff(t)()
	a, dir := newClaudeBoundedHelperAgent(t, "transient")
	var attempts []Attempt
	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: dir, BoundedWork: true, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) }})
	if err != nil || len(attempts) != 2 || result.Usage.CacheReadTokens != 50000 || result.PeakContextTokens != 30000 {
		t.Fatalf("attempts=%d result=%+v err=%v", len(attempts), result, err)
	}
	count, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || strings.TrimSpace(string(count)) != "2" {
		t.Fatalf("native calls=%q err=%v", count, err)
	}
}

func TestClaudeAgent_BoundedDispatchPolicyRefusesBeforeNativeLaunch(t *testing.T) {
	readErr := errors.New("cannot read recorded run usage")
	for _, tc := range []struct {
		name      string
		remaining int
		readErr   error
		callLimit int
		runLimit  int
		want      error
	}{
		{name: "exhausted", want: ErrInputTokenBudget},
		{name: "read-failure", readErr: readErr, want: readErr},
		{name: "caller-call-limit", remaining: 1500000, callLimit: 1000, want: ErrInputTokenBudget},
		{name: "caller-run-limit", remaining: 1500000, runLimit: 1000, want: ErrInputTokenBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			a := &claudeAgent{bin: "must-not-be-started"}
			_, err := a.Run(context.Background(), RunOpts{Prompt: "review", MaxInputTokens: tc.callLimit, MaxTotalInputTokens: tc.runLimit,
				ClaudeWorkBudget: &BoundedWorkBudget{MaxInputTokens: 150000, RemainingInputTokens: func() (int, error) {
					reads++
					return tc.remaining, tc.readErr
				}}})
			if !errors.Is(err, tc.want) || reads != 1 {
				t.Fatalf("reads=%d err=%v want=%v", reads, err, tc.want)
			}
		})
	}
}
