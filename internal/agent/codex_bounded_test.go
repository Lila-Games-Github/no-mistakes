package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newCodexBudgetFixture(t *testing.T, events string) (*codexAgent, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\ncat >prompt\nprintf '%s\\n' \"$@\" >argv\ncat <<'EVENTS'\n"+events+"\nEVENTS\n", "")
	return &codexAgent{bin: bin}, dir
}

const codexBudgetSuccess = `{"type":"thread.started","thread_id":"cold-fallback"}
{"type":"item.completed","item":{"type":"agent_message","text":"{\"summary\":\"complete\"}"}}
{"type":"turn.completed","usage":{"input_tokens":30000,"cached_input_tokens":25000,"output_tokens":10}}`

func TestCodexAgent_BoundedFallbackUsesFreshRemainingBudget(t *testing.T) {
	for _, purpose := range []string{"review", "review-fix", "ci-fix"} {
		t.Run(purpose, func(t *testing.T) {
			a, dir := newCodexBudgetFixture(t, codexBudgetSuccess)
			spent, reads := 0, 0
			var attempts []Attempt
			failed := &fallbackTestAgent{name: "claude", run: func() (*Result, error) {
				return &Result{UsageReported: true, Usage: TokenUsage{InputTokens: 50000, Reported: true}}, errors.New("claude exited: exit status 1")
			}}
			result, err := NewFallback([]Agent{failed, a}).Run(context.Background(), RunOpts{
				Prompt: "complete assigned duty", Purpose: purpose, CWD: dir, MaxInputTokens: 150000,
				Session:          &SessionRef{},
				ClaudeWorkBudget: &BoundedWorkBudget{MaxInputTokens: 150000, RemainingInputTokens: func() (int, error) { reads++; return 80000 - spent, nil }},
				OnAttempt: func(a Attempt) {
					attempts = append(attempts, a)
					if a.Result != nil {
						spent += a.Result.Usage.InputTokens
					}
				},
			})
			if err != nil || result == nil || result.Text != `{"summary":"complete"}` || result.Provider != "codex" || reads != 1 || spent != 80000 {
				t.Fatalf("result=%+v reads=%d spent=%d err=%v", result, reads, spent, err)
			}
			if result.SessionID != "" || result.Resumed || result.SessionUsageCumulative || len(attempts) != 2 || attempts[1].Session != nil {
				t.Fatalf("bounded fallback retained a native transcript: result=%+v attempts=%+v", result, attempts)
			}
			argv, err := os.ReadFile(filepath.Join(dir, "argv"))
			if err != nil {
				t.Fatal(err)
			}
			for _, arg := range []string{"--ephemeral", "--strict-config", "features.rollout_budget={enabled=true,limit_tokens=30000,prefill_token_weight=1.0,sampling_token_weight=0.0,reminder_at_remaining_tokens=[]}"} {
				if !strings.Contains(string(argv), arg+"\n") {
					t.Fatalf("native enforcement missing %q: %s", arg, argv)
				}
			}
		})
	}
}

func TestCodexAgent_BoundedBudgetCannotBeDisabled(t *testing.T) {
	for _, args := range [][]string{{"--disable", "rollout_budget"}, {"--disable=rollout_budget"}} {
		a := &codexAgent{bin: "must-not-be-started", extraArgs: args}
		_, err := a.Run(context.Background(), RunOpts{BoundedWork: true, MaxInputTokens: 150000})
		if err == nil || !strings.Contains(err.Error(), "cannot disable native input-budget enforcement") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestCodexAgent_BoundedNativeFailureDoesNotReplayEdits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	bin := writeFakeCodex(t, dir, "#!/bin/sh\ncat >/dev/null\nprintf edited >>edit.txt\ncat <<'EVENTS'\n"+codexBudgetSuccess+"\nEVENTS\nprintf '%s' '503 overloaded_error' >&2\nexit 1\n", "")
	a := &codexAgent{bin: bin}
	var attempts []Attempt
	result, err := a.Run(context.Background(), RunOpts{Prompt: "repair", CWD: dir, BoundedWork: true, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
		OnAttempt: func(a Attempt) { attempts = append(attempts, a) }})
	edits, readErr := os.ReadFile(filepath.Join(dir, "edit.txt"))
	if err == nil || readErr != nil || string(edits) != "edited" || len(attempts) != 1 || result == nil || result.Usage.InputTokens != 30000 || result.SessionID != "" {
		t.Fatalf("native failure lost usage or replayed work: edits=%q result=%+v attempts=%+v err=%v readErr=%v", edits, result, attempts, err, readErr)
	}
}

func TestCodexAgent_BoundedBudgetRefusesBeforeLaunch(t *testing.T) {
	readErr := errors.New("cannot read recorded usage")
	for _, tc := range []struct {
		name      string
		remaining int
		readErr   error
		want      error
	}{
		{"exhausted", 0, nil, ErrInputTokenBudget}, {"read-failure", 0, readErr, readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &codexAgent{bin: "must-not-be-started"}
			_, err := a.Run(context.Background(), RunOpts{BoundedWork: true, ClaudeWorkBudget: &BoundedWorkBudget{
				MaxInputTokens: 150000, RemainingInputTokens: func() (int, error) { return tc.remaining, tc.readErr },
			}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}

// This deliberately bad native stream must be refused even if a CLI ignores its
// budget flags and claims success, and also when it reports native exhaustion.
func TestCodexAgent_BoundedBudgetRejectsOverrunAndNativeExhaustion(t *testing.T) {
	for _, events := range []string{
		strings.Replace(codexBudgetSuccess, `"input_tokens":30000`, `"input_tokens":150001`, 1),
		`{"type":"error","message":"shared rollout token budget exhausted"}` + "\n" + `{"type":"turn.failed","error":{"message":"shared rollout token budget exhausted"}}`,
	} {
		a, dir := newCodexBudgetFixture(t, events)
		var attempts []Attempt
		_, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: dir, BoundedWork: true, MaxInputTokens: 150000, MaxTotalInputTokens: 1500000,
			OnAttempt: func(a Attempt) { attempts = append(attempts, a) }})
		if !errors.Is(err, ErrInputTokenBudget) || len(attempts) != 1 || attempts[0].Err == nil {
			t.Fatalf("over-budget stream accepted or retried: attempts=%+v err=%v", attempts, err)
		}
	}
}
