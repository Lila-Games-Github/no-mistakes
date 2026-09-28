package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/runmetrics"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type metricsAgent struct{}

func (*metricsAgent) Name() string { return "fake" }
func (*metricsAgent) Close() error { return nil }
func (*metricsAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{Text: "PRIVATE-RESPONSE"}, nil
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
	executor = NewExecutor(d, p, nil, &metricsAgent{}, []Step{review, ci}, func(event ipc.Event) {
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
