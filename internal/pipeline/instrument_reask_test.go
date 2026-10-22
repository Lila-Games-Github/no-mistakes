package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reaskingAgent models an adapter whose first structured answer is rejected and
// whose single corrective re-ask is accepted after a local repair.
type reaskingAgent struct{}

func (reaskingAgent) Name() string               { return "pi" }
func (reaskingAgent) Close() error               { return nil }
func (reaskingAgent) ReportsAgentAttempts() bool { return true }

func (reaskingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	now := time.Now()
	failed := &agent.StructuredOutputError{Agent: "pi", Text: "{", Err: errors.New("unexpected end of JSON input")}
	opts.OnAttempt(agent.Attempt{Agent: "pi", Err: failed, StartedAt: now, CompletedAt: now})
	result := &agent.Result{Output: json.RawMessage(`{"findings":[]}`), StructuredOutputRepaired: true}
	opts.OnAttempt(agent.Attempt{Agent: "pi", Result: result, StartedAt: now, CompletedAt: now, StructuredOutputReask: true})
	result.StructuredOutputReasks = 1
	return result, nil
}

func TestPerfRecording_CountsStructuredOutputReaskAndRepair(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: reaskingAgent{}, db: database, runID: run.ID, stepName: types.StepReview,
		round: func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "review", JSONSchema: json.RawMessage(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "plain text"}); err != nil {
		t.Fatal(err)
	}

	rows, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want one per attempt (4)", len(rows))
	}
	first, reask, plain := rows[0], rows[1], rows[3]
	if first.ExitStatus != "error" || first.FailureCategory != "parse" {
		t.Fatalf("rejected attempt = %s/%s, want error/parse", first.ExitStatus, first.FailureCategory)
	}
	if first.StructuredOutputReasks == nil || *first.StructuredOutputReasks != 0 {
		t.Fatalf("rejected attempt reasks = %v, want 0", first.StructuredOutputReasks)
	}
	if reask.StructuredOutputReasks == nil || *reask.StructuredOutputReasks != 1 {
		t.Fatalf("re-ask attempt reasks = %v, want 1", reask.StructuredOutputReasks)
	}
	if reask.StructuredOutputRepaired == nil || *reask.StructuredOutputRepaired != 1 {
		t.Fatalf("re-ask attempt repaired = %v, want 1", reask.StructuredOutputRepaired)
	}
	if plain.StructuredOutputReasks != nil || plain.StructuredOutputRepaired != nil {
		t.Fatal("an invocation without structured output recorded re-ask counters instead of unknown")
	}
}
