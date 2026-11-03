package pipeline

import (
	"context"
	"encoding/json"
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
		step    types.StepName
		purpose string
	}{
		{types.StepReview, "review"},
		{types.StepReview, "review-fix"},
		{types.StepCI, "ci"},
	} {
		t.Run(duty.purpose, func(t *testing.T) {
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
			recorded := &perfRecordingAgent{inner: native, db: database, runID: run.ID, stepName: duty.step, round: func() int { return 1 }}
			sctx := &StepContext{Ctx: context.Background(), DB: database, Run: run, Agent: recorded, WorkDir: dir, BriefDir: t.TempDir(), StepName: duty.step,
				Config: &config.Config{ContextBrief: true, Review: config.Review{ChunkThresholdLines: 800}}, Shared: &RunShared{contextBrief: "Small diff in a 1050-line file; inspect all assigned scope.\n" + patch}}
			result, err := sctx.RunAgent(agent.RunOpts{Prompt: "Complete the assigned work, retaining every finding and edit.", Purpose: duty.purpose, CWD: dir, JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"findings":{"type":"array"}},"required":["summary","findings"]}`)})
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
			if len(rows) != rounds+map[bool]int{true: 1}[prior > 0] {
				t.Fatalf("recorded %d calls, want %d plus prior usage", len(rows), rounds)
			}
			for _, row := range rows {
				if row.StartedAt == 1 {
					continue
				}
				used := perRoundInput(row) + row.CacheReadTokens
				if row.CacheCreationTokens != nil {
					used += *row.CacheCreationTokens
				}
				if used != 30000 || used > defaultReviewInputBudget {
					t.Fatalf("native call consumed %d", used)
				}
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
