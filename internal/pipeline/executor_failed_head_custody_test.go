package pipeline

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fixThenFailStep models a review step that committed fix rounds into the run
// worktree and then failed (for example on an unparseable agent answer).
type fixThenFailStep struct{}

func (fixThenFailStep) Name() types.StepName { return types.StepReview }

func (fixThenFailStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	for _, round := range []string{"fix one", "fix two"} {
		cmd := exec.Command("git", "commit", "--allow-empty", "-m", "no-mistakes(review): "+round)
		cmd.Dir = sctx.WorkDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, errors.New(string(out))
		}
	}
	head, err := exec.Command("git", "-C", sctx.WorkDir, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, err
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, strings.TrimSpace(string(head))); err != nil {
		return nil, err
	}
	return nil, errors.New("agent review: pi output parse: invalid character 'f' after object key:value pair")
}

// TestExecutor_FailedStepAfterFixRoundsKeepsHeadRecoverable: a step that fails
// after committing fix rounds must leave the pipeline's working head verified
// and pinned under the run's recovery ref, so branch_sync can always reach it.
func TestExecutor_FailedStepAfterFixRoundsKeepsHeadRecoverable(t *testing.T) {
	database, p, _, repo := setupTest(t)
	workDir := t.TempDir()
	initGitRepo(t, workDir)
	submitted, err := exec.Command("git", "-C", workDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", strings.TrimSpace(string(submitted)), strings.TrimSpace(string(submitted)))
	if err != nil {
		t.Fatal(err)
	}

	executor := NewExecutor(database, p, nil, nil, []Step{fixThenFailStep{}}, (&eventCollector{}).handler)
	if err := executor.Execute(context.Background(), run, repo, workDir); err == nil {
		t.Fatal("Execute succeeded, want the step failure")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	headOut, _ := execGitOutput(workDir, "rev-parse", "HEAD")
	if got.Status != types.RunFailed || got.HeadSHA != headOut || got.HeadSHA == strings.TrimSpace(string(submitted)) {
		t.Fatalf("run = status %s head %s, want failed at the fixed head %s", got.Status, got.HeadSHA, headOut)
	}
	if got.TerminalHeadVerifiedAt == nil {
		t.Fatal("failed run head was not verified at terminalization")
	}
	anchored, err := execGitOutput(workDir, "rev-parse", custody.RecoveryRef(run.ID))
	if err != nil || anchored != got.HeadSHA {
		t.Fatalf("recovery ref = %q (%v), want the failed run's head %s", anchored, err, got.HeadSHA)
	}
}

func execGitOutput(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}
