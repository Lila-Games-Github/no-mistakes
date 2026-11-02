package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

func TestPreparedContextBrief_PathRulesAndStablePrefix(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	base, err := git.Run(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "pkg/main.go", "package pkg\nfunc New() int { return 1 }\n")
	writeTestFile(t, dir, "pkg/main_test.go", "package pkg\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "feature")

	var prompts []string
	ag := &hangingAgent{name: "fake", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Text: "ok"}, nil
	}}
	sctx := &StepContext{
		Ctx: context.Background(), WorkDir: dir, BriefDir: t.TempDir(), Agent: ag,
		Run: &db.Run{BaseSHA: strings.TrimSpace(base)}, Shared: &RunShared{},
		Config: &config.Config{ContextBrief: true, Review: config.Review{PathInstructions: []config.PathInstruction{
			{Path: "pkg/**", Instructions: "Keep package invariants."},
			{Path: "docs/**", Instructions: "Document-only rule must stay absent."},
		}}, Commands: config.Commands{Test: "go test ./pkg", Lint: "go vet ./pkg"}},
	}
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "review step"}); err != nil {
		t.Fatal(err)
	}
	if command, err := sctx.PlannedTestCommand(context.Background()); err != nil || command != "go test ./'pkg'" {
		t.Fatalf("planned test = %q, error=%v", command, err)
	}
	if command, err := sctx.PlannedLintCommand(context.Background()); err != nil || command != "go vet ./'pkg'" {
		t.Fatalf("planned lint = %q, error=%v", command, err)
	}
	writeTestFile(t, dir, "pkg/main.go", "package pkg\nfunc New() int { return 2 }\n")
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "test step"}); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	stable := func(prompt string) string { return strings.SplitN(prompt, "\n---\nRunning log", 2)[0] }
	if stable(prompts[0]) != stable(prompts[1]) {
		t.Fatal("prepared context prefix changed between steps")
	}
	for _, want := range []string{"pkg/main.go", "pkg/main_test.go", "Keep package invariants.", "go test ./pkg", "go vet ./pkg", "return 1"} {
		if !strings.Contains(stable(prompts[0]), want) {
			t.Errorf("prepared brief missing %q", want)
		}
	}
	if strings.Contains(stable(prompts[0]), "Document-only rule") || strings.Contains(stable(prompts[1]), "return 2") {
		t.Fatal("brief included irrelevant rule or later working-tree change")
	}
	if !strings.HasSuffix(prompts[0], "review step") || !strings.HasSuffix(prompts[1], "test step") {
		t.Fatal("step-specific suffix was lost")
	}
}

func TestContextRunningLog_RecordsOutcomesAndDecisions(t *testing.T) {
	dir := t.TempDir()
	if err := appendContextOutcome(dir, "review", &StepOutcome{FixSummary: "fixed a race"}); err != nil {
		t.Fatal(err)
	}
	if err := appendContextDecision(dir, "review", "fix-final", []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}
	sctx := &StepContext{BriefDir: dir}
	got, err := sctx.contextRunningLog()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "fix=\"fixed a race\"") || !strings.Contains(got, "decision=fix-final; selected_findings=a,b") {
		t.Fatalf("running log lost outcome or decision: %q", got)
	}
}

func TestContextBrief_InitialCommitAndFreshSessions(t *testing.T) {
	if got := contextBriefBase(strings.Repeat("0", 40)); got != git.EmptyTreeSHA {
		t.Fatalf("initial-commit base = %s", got)
	}
	database, _, run, _ := setupTest(t)
	ag := &hangingAgent{name: "fake"}
	e := NewExecutor(database, nil, &config.Config{ContextBrief: true, SessionReuse: true}, ag, nil, nil)
	e.initializeRunScopes(run.ID)
	if e.sessions.enabled {
		t.Fatal("prepared context resumed a prior agent session")
	}
	e.config.ContextBrief = false
	e.initializeRunScopes(run.ID)
	if !e.sessions.enabled {
		t.Fatal("context opt-out lost existing session reuse")
	}
}

func TestPreparedContextBrief_FirstPushUsesBranchMergeBase(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	execGit(t, dir, "branch", "-M", "main")
	writeTestFile(t, dir, "unrelated.txt", "keep outside the brief\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "unrelated")
	execGit(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "feature.txt", "small change\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "feature")
	sctx := &StepContext{
		Ctx: context.Background(), WorkDir: dir, Run: &db.Run{BaseSHA: strings.Repeat("0", 40)},
		Repo: &db.Repo{DefaultBranch: "main"}, Config: &config.Config{ContextBrief: true},
	}
	brief, err := sctx.buildContextBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, `- "feature.txt"`) || strings.Contains(brief, `- "unrelated.txt"`) || strings.Contains(brief, "keep outside the brief") {
		t.Fatalf("first push brief has wrong scope:\n%s", brief)
	}
}
