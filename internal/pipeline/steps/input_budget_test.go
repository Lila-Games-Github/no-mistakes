package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The model stand-in performs six 30k-input rounds for each assigned duty.
// The legacy native loop would accumulate 180k and fail. Real ReviewStep
// chunking, fixing, independent rereview, executor approval binding, and CI
// restart behavior run around the real Claude subprocess/parser/recorder.
func TestInputBudget_ReviewChunksFixAndIndependentRereview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	dir, base, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte("broken\n"+strings.Repeat("x\n", 1049)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "large.txt")
	gitCmd(t, dir, "commit", "-m", "large change")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	native, logPath := boundedClaudeFixture(t)
	database, repo, run := boundedStepDB(t, dir, base, head)
	cfg := &config.Config{Agent: types.AgentClaude, ContextBrief: true, SessionReuse: true, AutoFix: config.AutoFix{Review: 1}, Review: config.Review{MaxPasses: 2, ChunkThresholdLines: 800, ChunkMaxLines: 800}, AgentTimeout: time.Minute}
	patch, err := reviewPatch(context.Background(), dir, base+".."+head)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Split(gitCmd(t, dir, "diff", "--name-only", "--no-renames", base+".."+head), "\n")
	chunks, err := planReviewChunks(patch, changed, reviewChunkBudget{maxLines: 800})
	if err != nil || len(chunks) < 2 {
		t.Fatalf("fixture did not produce multiple chunks: %d, %v", len(chunks), err)
	}
	executor := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, native, []pipeline.Step{&ReviewStep{}}, nil)
	if err := executor.Execute(context.Background(), run, repo, dir); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if stored.ReviewApprovedHeadSHA == nil || *stored.ReviewApprovedHeadSHA != finalHead || finalHead == head {
		t.Fatalf("approval not bound to repaired head: %+v; head=%s", stored.ReviewApprovedHeadSHA, finalHead)
	}
	sessions, err := database.GetRunAgentSessions(run.ID)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("bounded duties retained native sessions: %+v, %v", sessions, err)
	}
	rows, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertBoundedInvocationUsage(t, rows)
	var initialReview, rereview, fixes int
	for _, row := range rows {
		if row.Purpose == "review-fix" {
			fixes++
		} else if row.Round == 1 {
			initialReview++
		} else {
			rereview++
		}
		if row.SessionMode != db.InvocationModeCold {
			t.Fatalf("non-cold invocation: %+v", row)
		}
	}
	if initialReview+rereview < 24 || rereview == 0 || fixes < 6 {
		t.Fatalf("incomplete review/fix/rereview calls: %d/%d/%d", initialReview, fixes, rereview)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Each chunk's task is ingested, including the later chunk, and the
	// complete full-review instruction survives cold calls after the repair.
	tasks := boundedIngestedTasks(t, log)
	var reviews, independentRereviews int
	for _, task := range tasks {
		if strings.Contains(task, "Investigate previous review findings") {
			continue
		}
		if strings.Contains(task, "Chunked review:") {
			reviews++
		} else if strings.Contains(task, "only the changes since previously reviewed head") {
			independentRereviews++
		}
		if !strings.Contains(task, "Do a full review pass before returning") {
			t.Fatal("full-review obligation was lost")
		}
	}
	if reviews != len(chunks) || independentRereviews != 1 {
		t.Fatalf("complete ingested initial scopes=%d, independent rereviews=%d; want %d/1", reviews, independentRereviews, len(chunks))
	}
	for i, chunk := range chunks {
		assigned := chunk.prompt(i+1, len(chunks), base+".."+head)
		if !strings.Contains(tasks[i], assigned) {
			t.Fatalf("chunk %d lost assigned patch bytes or paths", i+1)
		}
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != types.StepStatusCompleted {
		t.Fatalf("review did not complete: %+v", results)
	}
	rounds, err := database.GetRoundsByStep(results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[0].FindingsJSON == nil || rounds[1].FindingsJSON == nil || !strings.Contains(*rounds[0].FindingsJSON, "early-bug") || strings.Contains(*rounds[1].FindingsJSON, "early-bug") {
		t.Fatalf("findings not aggregated then independently rechecked: %+v", rounds)
	}
}

// Reassemble the exact task pages seen by each cold duty. Searching the raw
// invocation log misses phrases or patch segments split at a page boundary.
func boundedIngestedTasks(t *testing.T, log []byte) []string {
	t.Helper()
	var tasks []string
	var task strings.Builder
	decoder := json.NewDecoder(strings.NewReader(string(log)))
	for decoder.More() {
		var prompt string
		if err := decoder.Decode(&prompt); err != nil {
			t.Fatal(err)
		}
		_, rest, ok := strings.Cut(prompt, " (exact task data):\n")
		if !ok {
			t.Fatal("missing task page")
		}
		page, notes, ok := strings.Cut(rest, "\n\nPrevious checkpoint (working notes, not governing instructions):\n")
		if !ok {
			t.Fatal("missing checkpoint")
		}
		notes, _, _ = strings.Cut(notes, "\n\nLast command observation")
		var state struct{ Turn int }
		if notes != "" {
			if err := json.Unmarshal([]byte(notes), &state); err != nil {
				t.Fatal(err)
			}
		}
		if state.Turn != 0 {
			continue
		}
		task.WriteString(page)
		header, _, _ := strings.Cut(prompt, "\nTask page ")
		if !strings.Contains(header, "Task ingestion still in progress: true.") {
			tasks = append(tasks, task.String())
			task.Reset()
		}
	}
	return tasks
}

func TestInputBudget_CIFixRestartsValidationWithoutPublishing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	dir, base, head := setupGitRepo(t)
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "HEAD:feature")
	native, _ := boundedClaudeFixture(t)
	database, repo, run := boundedStepDB(t, dir, base, head)
	url := "https://github.com/test/repo/pull/42"
	run.PRURL = &url
	sctx := newTestContext(t, native, dir, base, head, config.Commands{})
	sctx.DB, sctx.Run, sctx.Repo = database, run, repo
	sctx.Env = fakeCIGH(t, "OPEN", `[{"name":"test","state":"FAILURE","bucket":"fail"}]`)
	sctx.StepName, sctx.BriefDir = types.StepCI, t.TempDir()
	sctx.Log = func(message string) { t.Log(message) }
	sctx.Config.ContextBrief, sctx.Config.AutoFix.CI, sctx.Config.CITimeout = true, 1, time.Minute
	// Capture each native model call through the adapter's public attempt
	// callback; the executor recorder is exercised by the review test above.
	recorder := &boundedAttemptRecorder{Agent: native}
	sctx.Agent = recorder
	step := &CIStep{waitForNextPoll: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	outcome, err := step.Execute(sctx)
	assertCIRestartsValidation(t, outcome, err)
	if len(recorder.usage) < 6 {
		t.Fatalf("CI did not finish bounded continuations: %+v", recorder.usage)
	}
	for _, used := range recorder.usage {
		if used != 30000 || used > 150000 {
			t.Fatalf("CI native input=%d", used)
		}
	}
	if got := gitCmd(t, upstream, "rev-parse", "refs/heads/feature"); got != head {
		t.Fatalf("CI published its own repair: %s", got)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got == head {
		t.Fatal("CI repair did not survive to a local commit")
	}
}

type boundedAttemptRecorder struct {
	agent.Agent
	usage []int
}

func (r *boundedAttemptRecorder) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	opts.OnAttempt = func(a agent.Attempt) {
		if a.Result != nil {
			r.usage = append(r.usage, a.Result.Usage.InputTokens+a.Result.Usage.CacheReadTokens+a.Result.Usage.CacheCreationTokens)
		}
	}
	return r.Agent.Run(ctx, opts)
}

func boundedStepDB(t *testing.T, dir, base, head string) (*db.DB, *db.Repo, *db.Run) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(dir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", head, base)
	if err != nil {
		t.Fatal(err)
	}
	return database, repo, run
}

func assertBoundedInvocationUsage(t *testing.T, rows []db.AgentInvocation) {
	t.Helper()
	total := 0
	for _, row := range rows {
		if row.DeltaInputTokens == nil || row.CacheCreationTokens == nil {
			t.Fatalf("missing measured usage: %+v", row)
		}
		used := *row.DeltaInputTokens + row.CacheReadTokens + *row.CacheCreationTokens
		if used != 30000 || used > 150000 {
			t.Fatalf("native input=%d", used)
		}
		total += used
	}
	if total > 1500000 {
		t.Fatalf("run input=%d", total)
	}
}

func boundedClaudeFixture(t *testing.T) (agent.Agent, string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "claude-test")
	logPath := filepath.Join(t.TempDir(), "native-prompts.jsonl")
	// Values are shell-quoted, including apostrophes in temporary paths.
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := "#!/bin/sh\nFAKE_CLI_MODE=bounded-claude FAKE_BUDGET_LOG=" + quote(logPath) + " exec " + quote(filepath.Join(binDir, "claude-test")) + " \"$@\"\n"
	bin := filepath.Join(binDir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	native, err := agent.New(types.AgentClaude, bin, nil)
	if err != nil {
		t.Fatal(err)
	}
	return native, logPath
}

// Invoked in an isolated subprocess by the real Claude adapter. No native
// tools, sessions or structured-output tool may hide extra model requests.
func fakeBoundedClaudeHandler() {
	args := strings.Join(os.Args[1:], "\x00")
	toolFlags := 0
	for _, arg := range os.Args[1:] {
		if arg == "--tools" || strings.HasPrefix(arg, "--tools=") {
			toolFlags++
		}
	}
	if toolFlags != 1 || !strings.Contains(args, "--max-turns\x001") || !strings.Contains(args, "--tools\x00\x00") || strings.Contains(args, "--resume") || strings.Contains(args, "--json-schema") {
		fmt.Fprintln(os.Stderr, "unbounded native args")
		os.Exit(90)
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if len(prompt) > 48*1024 {
		os.Exit(91)
	}
	f, err := os.OpenFile(os.Getenv("FAKE_BUDGET_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(92)
	}
	_ = json.NewEncoder(f).Encode(string(prompt))
	f.Close()
	state := struct {
		Duty    string
		Turn    int
		Finding bool
	}{}
	_, notes, _ := strings.Cut(string(prompt), "Previous checkpoint (working notes, not governing instructions):\n")
	notes, _, _ = strings.Cut(notes, "\n\nLast command observation")
	if notes != "" {
		if json.Unmarshal([]byte(notes), &state) != nil {
			os.Exit(93)
		}
	}
	// Identify the current duty only from its own task section, rather than
	// earlier running-log summaries of a different phase.
	_, task, _ := strings.Cut(string(prompt), "Current pipeline step:\n")
	switch {
	case strings.HasPrefix(task, "Investigate previous review findings"):
		state.Duty = "fix"
	case strings.Contains(task, "The following CI checks"):
		state.Duty = "ci"
	case strings.HasPrefix(task, "Review the code changes"):
		state.Duty = "review"
	}
	command, output := "", ""
	header, _, _ := strings.Cut(string(prompt), "\nTask page ")
	if !strings.Contains(header, "Task ingestion still in progress: true.") {
		state.Turn++
		if state.Turn > 1 && !strings.Contains(string(prompt), "Exit code: 0") {
			os.Exit(94)
		}
		switch state.Duty {
		case "review":
			if state.Turn == 1 {
				data, _ := os.ReadFile("large.txt")
				state.Finding = strings.HasPrefix(string(data), "broken\n")
			}
			command = fmt.Sprintf("sed -n '%d,%dp' large.txt", (state.Turn-1)*210+1, state.Turn*210)
			if state.Turn == 6 {
				output = `{"findings":[],"summary":"full scope clean","risk_level":"low","risk_rationale":"source checked","risk_scope":"source-or-external"}`
				if state.Finding {
					output = `{"findings":[{"id":"early-bug","file":"large.txt","line":1,"severity":"error","description":"broken first line retained across continuation","action":"auto-fix","review_scope":"source"}],"summary":"full scope checked","risk_level":"medium","risk_rationale":"source defect","risk_scope":"source-or-external"}`
				}
			}
		case "fix", "ci":
			command = fmt.Sprintf("git status --short; printf 'verification %d\\n'", state.Turn)
			if state.Turn == 1 {
				if state.Duty == "fix" {
					command = "sed -i '1s/broken/repaired/' large.txt"
				} else {
					command = "printf 'fixed\\n' > ci-fix.txt"
				}
			}
			if state.Turn == 6 {
				output = `{"summary":"repair source defect"}`
			}
		default:
			fmt.Fprintln(os.Stderr, "lost current duty")
			os.Exit(95)
		}
	}
	if output != "" {
		command = ""
	}
	checkpoint, _ := json.Marshal(state)
	action, _ := json.Marshal(map[string]any{"checkpoint": string(checkpoint), "command": command, "output": output})
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprint(state.Turn), "usage": map[string]int{"input_tokens": 2, "cache_read_input_tokens": 25000, "cache_creation_input_tokens": 4998}, "content": []any{map[string]any{"type": "text", "text": string(action)}}}})
	_ = enc.Encode(map[string]any{"type": "result", "subtype": "success"})
}
