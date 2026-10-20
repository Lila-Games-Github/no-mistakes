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
)

func TestPlanReviewChunks_CoversEveryPatchByteAndPath(t *testing.T) {
	// A long line forces a split within a file; the next small file must still
	// appear after it. Reassembly checks the actual bytes, not just a count.
	longLine := "+" + strings.Repeat("x", reviewChunkMaxPatchBytes+100) + "\n"
	first := "diff --git a/large.txt b/large.txt\n--- a/large.txt\n+++ b/large.txt\n@@ -0,0 +1 @@\n" + longLine
	second := "diff --git a/small.txt b/small.txt\n--- a/small.txt\n+++ b/small.txt\n@@ -0,0 +1 @@\n+done\n"
	patch := first + second
	chunks, err := planReviewChunks(patch, []string{"large.txt", "small.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want long-file split plus small file", len(chunks))
	}
	var reconstructed strings.Builder
	var paths []string
	for _, chunk := range chunks {
		if chunk.bytes > reviewChunkMaxPatchBytes || len(chunk.parts) > reviewChunkMaxPaths {
			t.Fatalf("unbounded chunk: %d bytes, %d paths", chunk.bytes, len(chunk.parts))
		}
		for _, part := range chunk.parts {
			if len(paths) == 0 || paths[len(paths)-1] != part.path {
				paths = append(paths, part.path)
			}
			reconstructed.WriteString(part.text)
			if part.text != patch[reconstructed.Len()-len(part.text):reconstructed.Len()] {
				t.Fatal("chunk segment differs from original diff")
			}
		}
	}
	if got := reconstructed.String(); got != patch {
		t.Fatal("chunk reassembly lost or duplicated patch bytes")
	}
	if strings.Join(paths, ",") != "large.txt,small.txt" {
		t.Fatalf("covered paths = %q", paths)
	}
	if _, err := planReviewChunks(patch, []string{"large.txt"}); err == nil {
		t.Fatal("expected path-count mismatch to fail closed")
	}
}

func TestReviewStep_ChunkedReviewAggregatesEveryTurn(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	// These files are tracked in the feature commit; the large one splits,
	// and the later path must reach a review turn as well.
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("line = "+strings.Repeat("x", 480)+"\n", 610)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("critical change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "large.txt", "small.txt")
	gitCmd(t, dir, "commit", "-m", "large review fixture")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	call := 0
	ag := &mockAgent{name: "chunk-review", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		call++
		if !strings.Contains(opts.Prompt, "Chunked review:") || !strings.Contains(opts.Prompt, "BEGIN PATCH") {
			t.Fatal("chunk review did not receive its exact patch segment")
		}
		if call == 1 {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"large-bug","file":"large.txt","line":1,"severity":"error","description":"wrong value","action":"auto-fix","review_scope":"source"}],"risk_level":"high","risk_rationale":"wrong value","risk_scope":"source-or-external"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.ChunkThresholdLines = 501
	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if call < 2 {
		t.Fatalf("review calls = %d, want multiple chunks", call)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].ID != "large-bug" || findings.RiskLevel != "high" {
		t.Fatalf("merged findings = %+v", findings)
	}
	if !strings.Contains(ag.calls[len(ag.calls)-1].Prompt, "small.txt") {
		t.Fatal("later changed path was not assigned to a review turn")
	}
}

func TestReviewStep_ChunkedReviewFailsClosedOnMissingTurn(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("x\n", 600)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "large.txt")
	gitCmd(t, dir, "commit", "-m", "large review fixture")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "empty-review", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Text: "no findings"}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Review.ChunkThresholdLines = 501
	sctx.Config.IgnorePatterns = []string{"*.txt"}
	if _, err := (&ReviewStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "no structured findings") {
		t.Fatalf("missing chunk output error = %v", err)
	}
}

func TestReviewStep_ThresholdKeepsSmallReviewPromptIdentical(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	baselineAgent := &mockAgent{name: "baseline", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
	}}
	baseline := newTestContext(t, baselineAgent, dir, baseSHA, headSHA, config.Commands{})
	if _, err := (&ReviewStep{}).Execute(baseline); err != nil {
		t.Fatal(err)
	}
	thresholdAgent := &mockAgent{name: "threshold", runFn: baselineAgent.runFn}
	threshold := newTestContext(t, thresholdAgent, dir, baseSHA, headSHA, config.Commands{})
	threshold.Config.Review.ChunkThresholdLines = 501
	if _, err := (&ReviewStep{}).Execute(threshold); err != nil {
		t.Fatal(err)
	}
	if len(baselineAgent.calls) != 1 || len(thresholdAgent.calls) != 1 {
		t.Fatalf("small review call count changed: baseline=%d threshold=%d", len(baselineAgent.calls), len(thresholdAgent.calls))
	}
	if baselineAgent.calls[0].Prompt != thresholdAgent.calls[0].Prompt {
		t.Fatal("small review prompt changed with chunk threshold configured")
	}
}
