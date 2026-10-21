package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

const leanPreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref"}],"summary":"1 issue"}`

// leanRereviewContext prepares a post-fix re-review whose agent is listed in
// lean_turns. calls receives each prompt; script returns each call's result.
func leanRereviewContext(t *testing.T, script func(call int, opts agent.RunOpts) (*agent.Result, error)) (*pipeline.StepContext, *mockAgent, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	call := 0
	ag := &mockAgent{
		name: "pi",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			call++
			return script(call, opts)
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "make test"})
	sctx.Config.LeanTurns = config.DefaultGlobalConfig().LeanTurns
	sctx.RunbookDir = filepath.Join(t.TempDir(), "runbooks")
	sctx.Fixing = true
	sctx.ReviewStartingHeadSHA = headSHA
	sctx.PreviousFindings = leanPreviousFindings
	return sctx, ag, dir
}

func fixThenFindings(dir string, perCall map[int]Findings) func(int, agent.RunOpts) (*agent.Result, error) {
	return func(call int, opts agent.RunOpts) (*agent.Result, error) {
		if call == 1 {
			os.WriteFile(filepath.Join(dir, "review-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
		}
		j, _ := json.Marshal(perCall[call])
		return &agent.Result{Output: j}, nil
	}
}

func TestReviewStep_LeanRereviewReviewsTheDeltaThenRunsAFullPass(t *testing.T) {
	t.Parallel()
	var dir string
	sctx, ag, d := leanRereviewContext(t, func(call int, opts agent.RunOpts) (*agent.Result, error) {
		return fixThenFindings(dir, map[int]Findings{2: {Summary: "delta clean"}, 3: {Summary: "full clean"}})(call, opts)
	})
	dir = d
	startHead := sctx.ReviewStartingHeadSHA

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 3 {
		t.Fatalf("calls = %d, want fix + delta review + full review", len(ag.calls))
	}
	fix, delta, full := ag.calls[0], ag.calls[1], ag.calls[2]
	if delta.Purpose != "review-delta" || full.Purpose != "review" {
		t.Fatalf("purposes = %q, %q", delta.Purpose, full.Purpose)
	}
	for _, want := range []string{"Incremental re-review scope:", "`git diff " + startHead + "`", "review-fix.txt", "A full-scope review of the whole branch still runs"} {
		if !strings.Contains(delta.Prompt, want) {
			t.Errorf("delta prompt missing %q:\n%s", want, delta.Prompt)
		}
	}
	if strings.Contains(full.Prompt, "Incremental re-review scope:") {
		t.Error("the completing pass must be full scope")
	}
	for name, prompt := range map[string]string{"fix": fix.Prompt, "delta": delta.Prompt, "full": full.Prompt} {
		if !strings.Contains(prompt, "Repository run-book") || !strings.Contains(prompt, "configured commands.test: `make test`") || !strings.Contains(prompt, "Batch independent reads") {
			t.Errorf("%s prompt is missing the run-book", name)
		}
	}
	if outcome.NeedsApproval {
		t.Fatal("clean delta and full passes must not park")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil || findings.Summary != "full clean" {
		t.Fatalf("outcome findings = %s, want the full pass's result", outcome.Findings)
	}
	entries, err := os.ReadDir(sctx.RunbookDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run-book cache entries = %v (%v), want one cached pack", entries, err)
	}
}

func TestReviewStep_LeanRereviewParksOnABlockingDeltaFinding(t *testing.T) {
	t.Parallel()
	var dir string
	blocking := Findings{Items: []Finding{{ID: "delta-1", Severity: "error", File: "review-fix.txt", Description: "the fix is wrong", Action: "auto-fix"}}, Summary: "1 issue"}
	sctx, ag, d := leanRereviewContext(t, func(call int, opts agent.RunOpts) (*agent.Result, error) {
		return fixThenFindings(dir, map[int]Findings{2: blocking})(call, opts)
	})
	dir = d

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("calls = %d, want fix + delta review only", len(ag.calls))
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "delta-1") {
		t.Fatalf("outcome = %+v, want the delta finding parked", outcome)
	}
}

func TestReviewStep_LeanRereviewWithoutADeltaRunsTheFullPass(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := leanRereviewContext(t, func(call int, opts agent.RunOpts) (*agent.Result, error) {
		if call == 1 {
			// The fixer decides every finding was illegitimate and edits nothing.
			return &agent.Result{Output: json.RawMessage(`{"summary":"no change needed"}`)}, nil
		}
		j, _ := json.Marshal(Findings{Summary: "full clean"})
		return &agent.Result{Output: j}, nil
	})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 || ag.calls[1].Purpose != "review" || strings.Contains(ag.calls[1].Prompt, "Incremental re-review scope:") {
		t.Fatalf("calls = %d (second purpose %q), want fix + one full review", len(ag.calls), ag.calls[len(ag.calls)-1].Purpose)
	}
}

func TestReviewStep_UnlistedAgentKeepsTheExistingReviewShape(t *testing.T) {
	t.Parallel()
	var dir string
	sctx, ag, d := leanRereviewContext(t, func(call int, opts agent.RunOpts) (*agent.Result, error) {
		return fixThenFindings(dir, map[int]Findings{2: {Summary: "clean"}})(call, opts)
	})
	dir = d
	ag.name = "claude"

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("calls = %d, want the unchanged fix + full review", len(ag.calls))
	}
	for _, call := range ag.calls {
		if strings.Contains(call.Prompt, "Repository run-book") || strings.Contains(call.Prompt, "Incremental re-review scope:") || strings.Contains(call.Prompt, "Turn economy") {
			t.Fatalf("an agent not listed in lean_turns must get the unchanged prompt:\n%s", call.Prompt)
		}
	}
}

func TestReviewStep_LeanInitialReviewGetsTheRunbookButNoDeltaScope(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	clean, _ := json.Marshal(Findings{Summary: "clean"})
	ag := &mockAgent{name: "pi", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: clean}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.LeanTurns = config.DefaultGlobalConfig().LeanTurns
	sctx.ReviewStartingHeadSHA = headSHA

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("calls = %d, want one initial review", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "Repository run-book") || strings.Contains(ag.calls[0].Prompt, "Incremental re-review scope:") {
		t.Fatalf("initial review prompt:\n%s", ag.calls[0].Prompt)
	}
}

func TestRunbookPromptSection_OptOutKeepsProjectInstructionsOut(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Fleet captain\n\n## Secret ops\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "AGENTS.md")
	gitCmd(t, dir, "commit", "-qm", "agents")
	trusted := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContext(t, &mockAgent{name: "pi"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TrustedConfigSHA = trusted
	lean := config.DefaultGlobalConfig().LeanTurns

	if got := runbookPromptSection(sctx, lean, baseSHA); !strings.Contains(got, "AGENTS.md sections: Fleet captain; Secret ops") {
		t.Fatalf("run-book without the opt-out should index AGENTS.md:\n%s", got)
	}
	sctx.Config.DisableProjectSettings = true
	if got := runbookPromptSection(sctx, lean, baseSHA); strings.Contains(got, "AGENTS.md") || strings.Contains(got, "Fleet captain") {
		t.Fatalf("disable_project_settings must keep AGENTS.md out of the run-book:\n%s", got)
	}
}

func TestTestAndDocumentSteps_RunbookFollowsLeanTurns(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"pi", "claude"} {
		dir, baseSHA, headSHA := setupGitRepo(t)
		ag := &mockAgent{name: name, runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./x -count=1"],"testing_summary":"targeted check passed"}`)}, nil
		}}
		sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
		sctx.Config.LeanTurns = config.DefaultGlobalConfig().LeanTurns
		sctx.UserIntent = "Keep doctor checks green"
		if _, err := (&TestStep{}).Execute(sctx); err != nil {
			t.Fatal(err)
		}
		docPrompt := (&DocumentStep{}).buildPrompt(sctx, baseSHA, "none", false)
		for step, prompt := range map[string]string{"test": ag.calls[0].Prompt, "document": docPrompt} {
			has := strings.Contains(prompt, "Repository run-book") && strings.Contains(prompt, "Turn economy")
			if has != (name == "pi") {
				t.Errorf("%s %s prompt carries the run-book = %v, want %v", name, step, has, name == "pi")
			}
		}
	}
}
