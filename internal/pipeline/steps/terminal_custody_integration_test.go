package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A published feature can be rewritten by conflict repair. If the following
// guarded push refuses to discard remote work, terminal cleanup must leave the
// rewritten pipeline head reachable for branch_sync recovery.
func TestRejectedRewrittenPushSurvivesWorktreeCleanupAndRecovers(t *testing.T) {
	ctx := context.Background()
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, t.TempDir(), "init", "--bare", upstream)
	operator, base, submitted := setupGitRepo(t)
	gitCmd(t, operator, "remote", "add", "origin", upstream)
	gitCmd(t, operator, "push", "origin", "main")
	gitCmd(t, operator, "push", "origin", "feature")
	gate := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, t.TempDir(), "clone", "--bare", upstream, gate)
	runDir := filepath.Join(t.TempDir(), "run")
	gitCmd(t, gate, "--git-dir="+gate, "worktree", "add", "--detach", runDir, submitted)
	gitCmd(t, runDir, "config", "user.name", "test")
	gitCmd(t, runDir, "config", "user.email", "test@test.com")
	gitCmd(t, runDir, "remote", "set-url", "origin", upstream)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, runDir, base, submitted, config.Commands{})
	repo, err := sctx.DB.InsertRepo(operator, upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := sctx.DB.InsertRun(repo.ID, "feature", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Repo, sctx.Run = repo, run
	if err := os.WriteFile(filepath.Join(runDir, "published.txt"), []byte("published fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, runDir, "add", "published.txt")
	gitCmd(t, runDir, "commit", "-m", "published review fix")
	published := gitCmd(t, runDir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, published); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = published
	recordReviewApproval(t, sctx, published)
	if _, err := (&PushStep{}).Execute(sctx); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if got := gitCmd(t, upstream, "--git-dir="+upstream, "rev-parse", "refs/heads/feature"); got != published {
		t.Fatalf("initial remote head = %s, want %s", got, published)
	}

	// Conflict repair rewrites the feature hashes after main moved. Meanwhile
	// another commit reached the PR branch, which the rewrite did not include.
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "checkout", "feature")
	if err := os.WriteFile(filepath.Join(other, "remote.txt"), []byte("remote work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, other, "add", "remote.txt")
	gitCmd(t, other, "commit", "-m", "remote work")
	remoteHead := gitCmd(t, other, "rev-parse", "HEAD")
	gitCmd(t, other, "push", "origin", "feature")

	gitCmd(t, runDir, "checkout", "--detach", base)
	if err := os.WriteFile(filepath.Join(runDir, "main-moved.txt"), []byte("main moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, runDir, "add", "main-moved.txt")
	gitCmd(t, runDir, "commit", "-m", "main moved")
	gitCmd(t, runDir, "cherry-pick", submitted)
	gitCmd(t, runDir, "cherry-pick", published)
	if err := os.WriteFile(filepath.Join(runDir, "repair.txt"), []byte("conflict repair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, runDir, "add", "repair.txt")
	gitCmd(t, runDir, "commit", "-m", "CI conflict repair")
	rewritten := gitCmd(t, runDir, "rev-parse", "HEAD")
	if rewritten == published {
		t.Fatal("conflict repair did not rewrite the published head")
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, rewritten); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = rewritten
	recordReviewApproval(t, sctx, rewritten)

	// Execute through the terminalization boundary, which owns the recovery
	// anchor. The real PushStep supplies the guarded refusal.
	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, sctx.Agent, []pipeline.Step{&PushStep{}}, nil)
	if err := exec.Execute(ctx, sctx.Run, sctx.Repo, runDir); err == nil || !strings.Contains(err.Error(), "never incorporated") {
		t.Fatalf("guarded push = %v, want refusal of remote work the pipeline never incorporated", err)
	}
	terminal, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || terminal.Status != types.RunFailed || terminal.HeadSHA != rewritten || terminal.TerminalHeadVerifiedAt == nil {
		t.Fatalf("terminal run = %#v, err %v", terminal, err)
	}
	anchor := custody.RecoveryRef(sctx.Run.ID)
	if got := gitCmd(t, gate, "--git-dir="+gate, "rev-parse", anchor); got != rewritten {
		t.Fatalf("recovery anchor = %s, want %s", got, rewritten)
	}
	if got := gitCmd(t, upstream, "--git-dir="+upstream, "rev-parse", "refs/heads/feature"); got != remoteHead {
		t.Fatalf("refused push changed remote to %s, want %s", got, remoteHead)
	}
	gitCmd(t, gate, "--git-dir="+gate, "worktree", "remove", "--force", runDir)
	gitCmd(t, gate, "--git-dir="+gate, "gc", "--prune=now")
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("run worktree remains after cleanup: %v", err)
	}
	if got := gitCmd(t, gate, "--git-dir="+gate, "rev-parse", anchor); got != rewritten {
		t.Fatalf("cleanup lost rewritten head: %s", got)
	}

	service := &branchsync.Service{DB: sctx.DB, Repo: sctx.Repo, WorkDir: operator, GateDir: gate}
	state := service.InspectCached(ctx)
	if state.Safety == "blocked_recover_preserved_head_missing" {
		t.Fatalf("anchored head reported missing after cleanup: %#v", state)
	}
	recovered := service.Recover(ctx, true)
	if !recovered.Recovered {
		t.Fatalf("keep-local custody recovery = %#v", recovered)
	}
	if got := gitCmd(t, operator, "rev-parse", anchor); got != rewritten {
		t.Fatalf("recovery did not import rewritten head: %s", got)
	}
	if got := gitCmd(t, operator, "rev-parse", "HEAD"); got != submitted {
		t.Fatalf("keep-local recovery moved operator head to %s", got)
	}
}
