package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runmetrics"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type metricsAgent struct{}

func (*metricsAgent) Name() string { return "fake" }
func (*metricsAgent) Close() error { return nil }
func (*metricsAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{Text: "PRIVATE-RESPONSE", UsageReported: true,
		Usage:   agent.TokenUsage{InputTokens: 123, OutputTokens: 7, Reported: true},
		Metrics: &agent.InvocationMetrics{ModelRoundtrips: 2}}, nil
}

func TestExecutorMetricsRetainsFixGateAndCIRevalidation(t *testing.T) {
	d, p, run, repo := setupTest(t)
	reviews, cis := 0, 0
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		reviews++
		if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "PRIVATE-PROMPT", Purpose: "review"}); err != nil {
			return nil, err
		}
		return &StepOutcome{NeedsApproval: reviews == 1, Findings: `{"findings":[]}`}, nil
	}}
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
		cis++
		finish := runmetrics.Start(sctx.Ctx, runmetrics.CIPollWait)
		time.Sleep(2 * time.Millisecond)
		finish(nil, nil, runmetrics.Input{})
		if cis == 1 {
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		return &StepOutcome{}, nil
	}}
	var executor *Executor
	executor = NewExecutor(d, p, &config.Config{Review: config.Review{MaxPasses: 3}}, &metricsAgent{}, []Step{review, ci}, func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepReview, types.ActionFix, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if reviews != 3 || cis != 2 {
		t.Fatalf("telemetry changed execution: review=%d ci=%d", reviews, cis)
	}
	b, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), runmetrics.ArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	var a runmetrics.Artifact
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if !a.CollectionComplete || a.SchemaVersion != 1 || len(a.Invocations) != 3 {
		t.Fatalf("artifact: %+v", a)
	}
	if a.RunUsage.InputTokens == nil || *a.RunUsage.InputTokens != 369 || a.RunUsage.OutputTokens == nil || *a.RunUsage.OutputTokens != 21 || a.RunUsage.ModelRoundtrips == nil || *a.RunUsage.ModelRoundtrips != 6 || len(a.StepUsage) != 1 || a.StepUsage[0].Step != "review" {
		t.Fatalf("usage totals: run=%+v steps=%+v", a.RunUsage, a.StepUsage)
	}
	if len(a.Rounds) != 5 || a.Rounds[0].ReviewMaxPasses != 3 || a.Rounds[0].ReviewScope != "full" || a.Rounds[0].ReviewModel != "configured" || a.Rounds[1].ReviewScope != "delta" || a.Rounds[2].ReviewScope != "delta" {
		t.Fatalf("review receipt rounds: %+v", a.Rounds)
	}
	if a.Phases[0].ExecutionCycles != 3 || a.Phases[0].FixRounds != 1 || a.Phases[1].ExecutionCycles != 2 {
		t.Fatalf("cycles: %+v", a.Phases)
	}
	var gate, wait, contextSize bool
	for _, span := range a.Details {
		if span.Operation == runmetrics.HumanGate {
			gate = true
		}
		if span.Operation == runmetrics.CIPollWait && span.WallMS >= 1 {
			wait = true
		}
		if span.Operation == runmetrics.Agent && span.Input.ContextBytes != nil && *span.Input.ContextBytes >= len("PRIVATE-PROMPT") {
			contextSize = true
		}
	}
	if !gate || !wait || !contextSize {
		t.Fatalf("missing boundaries: gate=%t wait=%t input=%t", gate, wait, contextSize)
	}
	if strings.Contains(string(b), "PRIVATE-") {
		t.Fatal("prompt or response leaked into metrics")
	}
}

func TestReviewLimitParksAfterCIRestartWithoutThirdPass(t *testing.T) {
	d, p, run, repo := setupTest(t)
	reviews, cis, escalations := 0, 0, 0
	review := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		reviews++
		if reviews > 2 {
			t.Fatal("third review pass started")
		}
		action := "no-op"
		if reviews == 1 {
			action = "ask-user"
		}
		findings := fmt.Sprintf(`{"findings":[{"id":"review-1","severity":"warning","action":%q,"description":"remaining concern"}],"summary":"remaining concern"}`, action)
		return &StepOutcome{NeedsApproval: reviews == 1, Findings: findings, ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		cis++
		if cis == 1 {
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		return &StepOutcome{}, nil
	}}
	var executor *Executor
	executor = NewExecutor(d, p, nil, nil, []Step{review, ci}, func(event ipc.Event) {
		if event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
			return
		}
		if event.Findings != nil && strings.Contains(*event.Findings, "Review pass limit reached") {
			escalations++
			if !strings.Contains(*event.Findings, "remaining concern") {
				t.Fatal("escalation lost the remaining finding")
			}
			if !strings.Contains(*event.Findings, "Changes made after the final review pass have not been reviewed") {
				t.Fatal("escalation did not name the unreviewed changes")
			}
			if err := executor.Respond(types.StepReview, types.ActionFix, nil); err == nil {
				t.Fatal("fix accepted after review cap")
			}
			if err := executor.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := executor.Respond(types.StepReview, types.ActionFix, nil); err != nil {
			t.Fatal(err)
		}
	})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if reviews != 2 || cis != 2 || escalations != 1 {
		t.Fatalf("reviews=%d ci=%d escalation gates=%d", reviews, cis, escalations)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := d.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) != 2 {
		t.Fatalf("review rounds=%d, err=%v", len(rounds), err)
	}
}

func TestReviewLimitNamedFinalFixContinuesWithoutReviewerAndRecordsException(t *testing.T) {
	d, p, run, repo := setupTest(t)
	reviewCalls, ciCalls := 0, 0
	review := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		reviewCalls++
		if sctx.FinalFixWithoutReview {
			if !sctx.Fixing || !strings.Contains(sctx.PreviousFindings, "remaining concern") {
				t.Fatalf("final fixer lost selected finding: %+v", sctx)
			}
			return &StepOutcome{FixSummary: "resolve named concern", ReviewApprovedHeadSHA: run.HeadSHA, FinalFixUnreviewed: true}, nil
		}
		if reviewCalls > 2 {
			t.Fatal("reviewer called beyond pass cap")
		}
		action := "no-op"
		if reviewCalls == 1 {
			action = "ask-user"
		}
		findings := fmt.Sprintf(`{"findings":[{"id":"review-1","severity":"warning","action":%q,"description":"remaining concern"}],"summary":"remaining concern"}`, action)
		return &StepOutcome{NeedsApproval: reviewCalls == 1, Findings: findings, ReviewApprovedHeadSHA: run.HeadSHA}, nil
	}}
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		ciCalls++
		if ciCalls == 1 {
			return &StepOutcome{RestartFrom: types.StepReview}, nil
		}
		return &StepOutcome{}, nil
	}}
	var executor *Executor
	executor = NewExecutor(d, p, nil, nil, []Step{review, ci}, func(event ipc.Event) {
		if event.Status == nil || *event.Status != string(types.StepStatusAwaitingApproval) {
			return
		}
		if event.Findings != nil && strings.Contains(*event.Findings, "Review pass limit reached") {
			if err := executor.Respond(types.StepReview, types.ActionFixFinal, []string{"review-1"}); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := executor.Respond(types.StepReview, types.ActionFix, []string{"review-1"}); err != nil {
			t.Fatal(err)
		}
	})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if reviewCalls != 3 || ciCalls != 2 {
		t.Fatalf("review calls=%d (last fixer-only), CI calls=%d", reviewCalls, ciCalls)
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds, err := d.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) != 3 || rounds[2].Trigger != "final_fix_unreviewed" || rounds[2].ReviewedHeadSHA != nil {
		t.Fatalf("final fix round = %+v, error=%v", rounds, err)
	}
	artifact, err := runmetrics.Build(d, run.ID, p.RunLogDir(run.ID), time.Now())
	if err != nil || !artifact.UnreviewedFinalFix {
		t.Fatalf("receipt omitted unreviewed final fix: %+v, %v", artifact, err)
	}
}

func TestExecutorMetricsStorageFailureDoesNotChangeSuccessfulRun(t *testing.T) {
	d, p, run, repo := setupTest(t)
	// Block only the optional journal, leaving step logs usable.
	dir := p.RunLogDir(run.ID)
	if err := os.MkdirAll(filepath.Join(dir, runmetrics.JournalName), 0o755); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(d, p, nil, nil, []Step{newPassStep(types.StepTest)}, nil)
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunCompleted {
		t.Fatalf("run changed by telemetry failure: %v %+v", err, stored)
	}
}

func TestExecutorMetricsPreservesStepContextBetweenFixRounds(t *testing.T) {
	d, p, run, repo := setupTest(t)
	type stepKey struct{}
	calls := 0
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		if calls == 1 {
			sctx.Ctx = context.WithValue(sctx.Ctx, stepKey{}, "phase-owned-context")
			return &StepOutcome{NeedsApproval: true}, nil
		}
		if got := sctx.Ctx.Value(stepKey{}); got != "phase-owned-context" {
			t.Fatalf("observation replaced step context: %v", got)
		}
		return &StepOutcome{}, nil
	}}
	var executor *Executor
	executor = NewExecutor(d, p, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			if err := executor.Respond(types.StepTest, types.ActionFix, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("execution changed: %d calls", calls)
	}
}
