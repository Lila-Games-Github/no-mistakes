package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The incident had a small diff in large files and six cold Claude model
// messages of about 30k input, mostly cache reads. Patch chunking never fired.
// Exercise the real subprocess/parser/recording seam, not a fake budget check.
func TestInputBudget_SixModelRoundsCompleteInBoundedCalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	for _, duty := range []struct {
		step     types.StepName
		purpose  string
		fallback bool
	}{
		{types.StepReview, "review", false},
		{types.StepReview, "review-fix", false},
		{types.StepCI, "ci", false},
		{types.StepReview, "review", true},
		{types.StepReview, "review-fix", true},
		{types.StepCI, "ci", true},
	} {
		t.Run(fmt.Sprintf("%s/fallback=%t", duty.purpose, duty.fallback), func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			dir := t.TempDir()
			for _, args := range [][]string{{"init"}, {"config", "user.email", "budget@example.test"}, {"config", "user.name", "Budget fixture"}} {
				if _, err := git.Run(context.Background(), dir, args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte(strings.Repeat("unchanged context\n", 1050)), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(context.Background(), dir, "add", "source.txt"); err != nil {
				t.Fatal(err)
			}
			if _, err := git.Run(context.Background(), dir, "-c", "commit.gpgsign=false", "commit", "-m", "baseline"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte(strings.Repeat("changed context\n", 77)+strings.Repeat("unchanged context\n", 973)), 0o644); err != nil {
				t.Fatal(err)
			}
			patch, err := git.Run(context.Background(), dir, "diff", "--unified=0")
			if err != nil {
				t.Fatal(err)
			}
			rounds, prior := 6, 0
			if duty.step == types.StepCI {
				rounds, prior = 28, 222615 // authoritative CI run's already-spent input
				if _, err := database.InsertAgentInvocation(db.AgentInvocation{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "claude", SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok", DeltaInputTokens: &prior}); err != nil {
					t.Fatal(err)
				}
			}
			final := `{"summary":"all assigned work complete","findings":[{"id":"early","file":"source.txt","line":77,"description":"retained early finding"}]}`
			for i := 1; i <= rounds; i++ {
				decision := map[string]any{"checkpoint": fmt.Sprintf("progress-%d; retained early finding; remaining scope must be checked", i), "command": fmt.Sprintf("sed -n '%d,%dp' source.txt", (i-1)*210+1, i*210), "output": ""}
				if duty.purpose != "review" && i == 1 {
					decision["command"] = "printf 'preserved repair\\n' >> source.txt"
				}
				if i == rounds {
					decision["command"] = ""
					decision["output"] = final
				}
				payload, _ := json.Marshal(decision)
				event, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprint(i), "usage": map[string]int{"input_tokens": 2, "cache_read_input_tokens": 25000, "cache_creation_input_tokens": 4998}, "content": []any{map[string]any{"type": "text", "text": string(payload)}}}})
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("event-%d", i)), append(event, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			script := `#!/bin/sh
set -eu
bounded=no
tools=no
while [ "$#" -gt 0 ]; do
  if [ "$1" = --max-turns ] && [ "${2:-}" = 1 ]; then bounded=yes; fi
  if [ "$1" = --tools ] && [ "${2-unset}" = '' ]; then tools=yes; fi
  if [ "$1" = --resume ]; then exit 91; fi
  shift
done
if [ "$bounded" = no ]; then
  cat >/dev/null
  for n in 1 2 3 4 5 6; do cat "event-$n"; done
else
  test "$tools" = yes
  n=1
  if [ -f counter ]; then n=$(($(cat counter)+1)); fi
  printf '%s' "$n" >counter
  cat >"prompt-$n"
  if [ "$n" -gt 1 ]; then previous=$((n-1)); grep -q "progress-$previous" "prompt-$n"; fi
  cat "event-$n"
fi
printf '%s\n' '{"type":"result","subtype":"success"}'
`
			bin := filepath.Join(dir, "claude")
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			native, err := agent.New(types.AgentClaude, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			var selected agent.Agent = native
			if duty.fallback {
				selected = agent.NewFallback([]agent.Agent{&budgetUnavailableCodex{}, native})
			}
			recorded := &perfRecordingAgent{inner: selected, db: database, runID: run.ID, stepName: duty.step, round: func() int { return 1 }}
			sctx := &StepContext{Ctx: context.Background(), DB: database, Run: run, Agent: recorded, WorkDir: dir, BriefDir: t.TempDir(), StepName: duty.step,
				Config: &config.Config{ContextBrief: true, Review: config.Review{ChunkThresholdLines: 800}}, Shared: &RunShared{contextBrief: "Small diff in a 1050-line file; inspect all assigned scope.\n" + patch}}
			opts := agent.RunOpts{Prompt: "Complete the assigned work, retaining every finding and edit.", Purpose: duty.purpose, CWD: dir, JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"findings":{"type":"array"}},"required":["summary","findings"]}`)}
			var result *agent.Result
			if duty.purpose == "review-fix" {
				// Recovery can leave a Claude transcript minted before bounded work.
				// It must be retired even when Claude is a secondary provider.
				if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "claude", "old-unbounded-session"); err != nil {
					t.Fatal(err)
				}
				sctx.Sessions = NewRunSessions(database, run.ID, recorded, true)
				result, err = sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, opts)
				stored, readErr := database.GetRunAgentSessions(run.ID)
				if readErr != nil || len(stored) != 0 {
					t.Fatalf("bounded fixer retained native session: %+v, %v", stored, readErr)
				}
			} else {
				result, err = sctx.RunAgent(opts)
			}
			if err != nil {
				t.Fatalf("six-round task should complete inside bounded calls: %v", err)
			}
			if string(result.Output) != final {
				t.Fatalf("lost final findings: %s", result.Output)
			}
			rows, err := database.GetAgentInvocationsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != rounds+map[bool]int{true: 1}[prior > 0]+map[bool]int{true: 1}[duty.fallback] {
				t.Fatalf("recorded %d calls, want %d plus prior usage", len(rows), rounds)
			}
			findings := 0
			for _, row := range rows {
				if row.StartedAt == 1 || row.Agent == "codex" {
					continue
				}
				if row.SessionMode != db.InvocationModeCold {
					t.Fatalf("bounded native call resumed a session: %+v", row)
				}
				if row.FindingCount != nil {
					findings += *row.FindingCount
				}
				used := perRoundInput(row) + row.CacheReadTokens
				if row.CacheCreationTokens != nil {
					used += *row.CacheCreationTokens
				}
				if used != 30000 || used > defaultReviewInputBudget {
					t.Fatalf("native call consumed %d", used)
				}
			}
			if findings != 1 || rows[len(rows)-1].FindingCount == nil || *rows[len(rows)-1].FindingCount != 1 {
				t.Fatalf("final native attempt lost findings or duplicated them: count=%d last=%+v", findings, rows[len(rows)-1])
			}
			spent, err := sctx.recordedRunInputTokens()
			if err != nil || spent != prior+rounds*30000 || spent > defaultRunInputBudget {
				t.Fatalf("run input = %d, %v; cache usage must remain accounted", spent, err)
			}
			if duty.purpose != "review" {
				contents, _ := os.ReadFile(filepath.Join(dir, "source.txt"))
				if !strings.Contains(string(contents), "preserved repair") {
					t.Fatal("continuation lost a repair")
				}
			}
		})
	}
}

// Provider failure can still report consumed input. The next provider must use
// what remains after that attempt, not the duty's original run allowance.
type budgetUnavailableCodex struct{ input int }

func (*budgetUnavailableCodex) Name() string                { return "codex" }
func (*budgetUnavailableCodex) Close() error                { return nil }
func (*budgetUnavailableCodex) SupportsSessionResume() bool { return true }
func (a *budgetUnavailableCodex) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{UsageReported: true, Usage: agent.TokenUsage{InputTokens: a.input, Reported: true}}, errors.New("codex exited: provider unavailable")
}

func TestInputBudget_ClaudeFallbackRefreshesRemainingRunAllowance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-resume=%t", resume), func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			prior := defaultRunInputBudget - 80000
			if _, err := database.InsertAgentInvocation(db.AgentInvocation{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "claude", SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok", DeltaInputTokens: &prior}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			script := `#!/bin/sh
set -eu
cat >/dev/null
n=1
if [ -f counter ]; then n=$(($(cat counter)+1)); fi
printf '%s' "$n" >counter
printf '%s\n' '{"type":"assistant","message":{"id":"one-call","usage":{"input_tokens":2,"cache_read_input_tokens":25000,"cache_creation_input_tokens":4998},"content":[{"type":"text","text":"{\"checkpoint\":\"one repair preserved; verification outstanding\",\"command\":\"printf repaired >> repair.txt\",\"output\":\"\"}"}]}}'
printf '%s\n' '{"type":"result","subtype":"success"}'
`
			bin := filepath.Join(dir, "claude")
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			native, err := agent.New(types.AgentClaude, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			failedCalls, failedInput := 1, 50000
			if resume {
				failedCalls, failedInput = 2, 25000
				if err := database.UpsertRunAgentSession(run.ID, string(SessionRoleFixer), "codex", "old-codex-session"); err != nil {
					t.Fatal(err)
				}
			}
			selected := agent.NewFallback([]agent.Agent{&budgetUnavailableCodex{input: failedInput}, native})
			recorded := &perfRecordingAgent{inner: selected, db: database, runID: run.ID, stepName: types.StepReview, round: func() int { return 1 }}
			sctx := &StepContext{Ctx: context.Background(), DB: database, Run: run, Agent: recorded, StepName: types.StepReview, Config: &config.Config{ContextBrief: true}, Shared: &RunShared{contextBrief: "Complete the assigned repair and verification."}, Sessions: NewRunSessions(database, run.ID, recorded, true)}
			_, err = sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, agent.RunOpts{Prompt: "Repair and verify all assigned findings.", Purpose: "review-fix", CWD: dir})
			if !errors.Is(err, agent.ErrInputTokenBudget) {
				t.Fatalf("exhausted run accepted partial work: %v", err)
			}
			count, readErr := os.ReadFile(filepath.Join(dir, "counter"))
			if readErr != nil || string(count) != "1" {
				t.Fatalf("stale allowance launched another native call: count=%q err=%v", count, readErr)
			}
			contents, readErr := os.ReadFile(filepath.Join(dir, "repair.txt"))
			if readErr != nil || string(contents) != "repaired" {
				t.Fatalf("repair was lost or replayed: %q err=%v", contents, readErr)
			}
			rows, err := database.GetAgentInvocationsByRun(run.ID)
			if err != nil || len(rows) != 1+failedCalls+1 {
				t.Fatalf("failed attempts lost or native usage duplicated: rows=%d err=%v", len(rows), err)
			}
			spent, err := sctx.recordedRunInputTokens()
			if err != nil || spent != defaultRunInputBudget {
				t.Fatalf("remaining allowance missed failed-attempt usage: spent=%d err=%v", spent, err)
			}
		})
	}
}

type budgetNativeCodex struct {
	input int
	calls []agent.RunOpts
}

func (*budgetNativeCodex) Name() string                { return "codex" }
func (*budgetNativeCodex) Close() error                { return nil }
func (*budgetNativeCodex) SupportsSessionResume() bool { return true }
func (a *budgetNativeCodex) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls = append(a.calls, opts)
	result := &agent.Result{Text: "complete", UsageReported: true, Usage: agent.TokenUsage{InputTokens: a.input, Reported: true}}
	if opts.Session != nil {
		result.SessionID = "codex-fixer"
		result.Resumed = opts.Session.ID != ""
	}
	return result, nil
}

func TestInputBudget_ClaudeDispatchPolicyPreservesOtherProviders(t *testing.T) {
	for _, step := range []types.StepName{types.StepReview, types.StepCI} {
		t.Run(string(step), func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			input, limit := 30000, defaultReviewInputBudget
			if step == types.StepCI {
				// Native Codex CI keeps the existing remaining-run ceiling.
				input, limit = 200000, defaultRunInputBudget
			}
			primary := &budgetNativeCodex{input: input}
			secondary, err := agent.New(types.AgentClaude, "must-not-be-started", nil)
			if err != nil {
				t.Fatal(err)
			}
			selected := agent.NewFallback([]agent.Agent{primary, secondary})
			recorded := &perfRecordingAgent{inner: selected, db: database, runID: run.ID, stepName: step, round: func() int { return 1 }}
			sctx := &StepContext{Ctx: context.Background(), DB: database, Run: run, Agent: recorded, StepName: step, Config: &config.Config{ContextBrief: true}, Shared: &RunShared{contextBrief: "complete assigned work"}, Sessions: NewRunSessions(database, run.ID, recorded, true)}
			for turn := 0; turn < 2; turn++ {
				opts := agent.RunOpts{Prompt: "Complete this duty."}
				if step == types.StepReview {
					_, err = sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, opts)
				} else {
					_, err = sctx.RunAgent(opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				call := primary.calls[turn]
				if call.BoundedWork || call.MaxTotalInputTokens != 0 || call.MaxInputTokens != limit {
					t.Fatalf("Claude policy altered native Codex execution: bounded=%t total=%d input=%d want=%d", call.BoundedWork, call.MaxTotalInputTokens, call.MaxInputTokens, limit)
				}
				if step == types.StepReview {
					wantID := ""
					if turn > 0 {
						wantID = "codex-fixer"
					}
					if call.Session == nil || call.Session.ID != wantID {
						t.Fatalf("Codex fixer session was dropped: %+v", call.Session)
					}
				} else {
					limit -= input
				}
			}
		})
	}
}
