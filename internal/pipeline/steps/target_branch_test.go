package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

func TestRunTargetBranchTakesPrecedenceOverConfigAndDefault(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	target := "proto/godot/frog-pile"
	sctx.Run.TargetBranch = &target
	sctx.Config.PR.BaseBranch = "configured-integration"
	sctx.Repo.DefaultBranch = "main"

	if got := runTargetBranch(sctx); got != target {
		t.Fatalf("runTargetBranch() = %q, want durable target %q", got, target)
	}
	sctx.Run.TargetBranch = nil
	if got := runTargetBranch(sctx); got != "configured-integration" {
		t.Fatalf("legacy run target = %q, want configured integration", got)
	}
	sctx.Config.PR.BaseBranch = ""
	if got := runTargetBranch(sctx); got != "main" {
		t.Fatalf("default run target = %q, want main", got)
	}
}

// This graph reproduces the expensive scope explosion that motivated explicit
// targets: the long-lived integration branch contains a body of work not on
// main, and the feature adds one file on top. Comparing the feature to main
// reviews the entire integration history; comparing it to the selected target
// reviews only the feature commit.
func TestReviewStep_TargetBranchExcludesIntegrationHistoryFromScope(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init", "--initial-branch=main")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "root.txt")
	gitCmd(t, dir, "commit", "-m", "root")
	rootSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	const target = "proto/godot/frog-pile"
	gitCmd(t, dir, "checkout", "-b", target)
	for i := 0; i < 12; i++ {
		name := filepath.Join(dir, fmt.Sprintf("integration-%02d.txt", i))
		if err := os.WriteFile(name, []byte("integration baseline\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "integration baseline")
	targetSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature/targeted")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "feature.txt")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	// Advance main independently too, matching a live integration branch that
	// has diverged in both directions from the forge default.
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "main-only.txt"), []byte("default divergence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "main-only.txt")
	gitCmd(t, dir, "commit", "-m", "main divergence")
	gitCmd(t, dir, "checkout", "feature/targeted")

	defaultBase := mergeBaseWithDefaultBranch(context.Background(), dir, "main")
	if defaultBase != rootSHA {
		t.Fatalf("default merge base = %s, want root %s", defaultBase, rootSHA)
	}
	defaultScope := reviewWorkload(context.Background(), dir, defaultBase, headSHA)
	if defaultScope == nil || defaultScope.Files != 13 {
		t.Fatalf("default-branch review workload = %#v, want 13 files", defaultScope)
	}
	if got := resolveIntentBaseSHA(context.Background(), dir, rootSHA, target); got != targetSHA {
		t.Fatalf("intent comparison base = %s, want target branch tip %s instead of reachable legacy base %s", got, targetSHA, rootSHA)
	}

	ag := &mockAgent{
		name: "scope-capture",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			output, _ := json.Marshal(Findings{Summary: "clean", RiskLevel: "low", RiskRationale: "one-file feature"})
			return &agent.Result{Output: output}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, strings.Repeat("0", 40), headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature/targeted"
	sctx.Run.TargetBranch = stringPtr(target)
	sctx.Config.PR.BaseBranch = "configured-wrong-base"

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("review agent calls = %d, want 1", len(ag.calls))
	}
	call := ag.calls[0]
	if call.Workload == nil || call.Workload.Files != 1 || call.Workload.Lines != 1 {
		t.Fatalf("targeted review workload = %#v, want one file and one line", call.Workload)
	}
	for _, want := range []string{"base commit: " + targetSHA, "target branch: " + target} {
		if !strings.Contains(call.Prompt, want) {
			t.Errorf("review prompt missing %q:\n%s", want, call.Prompt)
		}
	}
}

func stringPtr(value string) *string { return &value }
