package eval

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

type replayUsageAgent struct {
	results []*agent.Result
}

func (a *replayUsageAgent) Name() string { return "usage-probe" }
func (a *replayUsageAgent) Close() error { return nil }
func (a *replayUsageAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	result := a.results[0]
	a.results = a.results[1:]
	return result, nil
}

func TestObservedAgentAggregatesEveryChunkUsage(t *testing.T) {
	inner := &replayUsageAgent{results: []*agent.Result{
		{Model: "model-a", UsageReported: true, Usage: agent.TokenUsage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 4}},
		{Model: "model-a", UsageReported: true, Usage: agent.TokenUsage{InputTokens: 20, OutputTokens: 3, CacheReadTokens: 8}},
	}}
	observed := &observedAgent{inner: inner}
	for range 2 {
		if _, err := observed.Run(context.Background(), agent.RunOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	if observed.usageMissing || observed.modelMismatch || observed.model != "model-a" {
		t.Fatalf("observation status = missing %v, mismatch %v, model %q", observed.usageMissing, observed.modelMismatch, observed.model)
	}
	if observed.usage.InputTokens != 30 || observed.usage.OutputTokens != 5 || observed.usage.CacheReadTokens != 12 {
		t.Fatalf("aggregated usage = %+v", observed.usage)
	}
	if observed.result == nil || observed.result.Usage.OutputTokens != 3 {
		t.Fatal("last chunk result was not retained")
	}
}
