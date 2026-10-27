package eval

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const resumeFindings = `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`

// TestReplayResumeRetriesOnlyTheFailedSlotAndKeepsTheCohort is the recovery
// contract of the eval tooling: one provider failure must not cost the
// completed samples of a long paired replay. The resumed batch replans exactly
// the slots whose latest attempt is missing or failed, stays in the recorded
// session's cohort and candidate identity, and records the retry as its own
// attempt instead of overwriting the failed row.
func TestReplayResumeRetriesOnlyTheFailedSlotAndKeepsTheCohort(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	second := addSecondCapturedCase(t, ctx, p, sourceDB, repo, run, store)

	// The first pass fails exactly one slot, the second case's first repeat.
	marker := installFlakyReviewAgent(t, p)

	session, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 2})
	if err == nil || !strings.Contains(err.Error(), "1 replay invocation(s) failed") {
		t.Fatalf("first pass error = %v, want the reported failed invocation", err)
	}
	if len(evaluations) != 4 || session.ID == "" {
		t.Fatalf("first pass = session %#v over %d evaluation(s), want four planned slots", session, len(evaluations))
	}
	failed := failedSlot(t, store, session.ID)
	if failed.CaseID != second.ID || failed.Repeat != 1 {
		t.Fatalf("failed slot = %#v, want the flaky agent's second.go invocation at repeat 1", failed)
	}

	// The provider recovers; the operator retries nothing but the failure.
	installFakeReviewAgent(t, p, resumeFindings)
	var plan ReplayPlan
	resumed, retried, err := Replay(ctx, store, ReplayOptions{
		ResumeSessionID: session.ID,
		OnPlan:          func(p ReplayPlan) { plan = p },
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ID != session.ID || resumed.Cohort != session.Cohort || resumed.Candidate != session.Candidate {
		t.Fatalf("resumed session %#v, want the recorded session %#v", resumed, session)
	}
	if !plan.Resumed || len(plan.Slots) != 1 || plan.Total != 4 {
		t.Fatalf("resume plan = %#v, want one retried slot out of four", plan)
	}
	if plan.Slots[0].CaseID != second.ID || plan.Slots[0].Repeat != 1 || plan.Slots[0].Attempt != 2 {
		t.Fatalf("resume plan slot = %#v, want the failed slot at attempt 2", plan.Slots[0])
	}
	if len(retried) != 1 || retried[0].Status != "completed" || retried[0].Attempt != 2 {
		t.Fatalf("resumed evaluations = %#v, want one completed second attempt", retried)
	}

	// The failed attempt stays recorded as its own row and file, and the
	// completed siblings were neither replanned nor rewritten.
	slotDir := filepath.Join(caseByID(t, store, second.ID).Dir, "evals", session.ID, candidatePathPart(session.Candidate))
	failedAttempt := readAttempt(t, filepath.Join(slotDir, "repeat-001.json"))
	if failedAttempt.Status != "failed" || failedAttempt.Attempt != 1 || failedAttempt.GoldCount != 1 {
		t.Fatalf("first attempt = %#v, want the failed repeat kept under its original name", failedAttempt)
	}
	retryAttempt := readAttempt(t, filepath.Join(slotDir, "repeat-001-attempt-02.json"))
	if retryAttempt.Status != "completed" || retryAttempt.Attempt != 2 || retryAttempt.Repeat != 1 {
		t.Fatalf("retry attempt = %#v, want the completed retry recorded beside it", retryAttempt)
	}
	if _, err := os.Stat(filepath.Join(slotDir, "repeat-002-attempt-02.json")); !os.IsNotExist(err) {
		t.Fatalf("the completed repeat was replayed as a second attempt: %v", err)
	}
	rows := countEvaluationRows(t, store, session.ID)
	if rows != 5 {
		t.Fatalf("session rows = %d, want 4 planned slots plus 1 retried attempt", rows)
	}
	if got := countEvaluationRowsForCase(t, store, session.ID, second.ID); got != 3 {
		t.Fatalf("second case rows = %d, want its two repeats plus the retry", got)
	}

	// The report keeps one authoritative row per slot, so the recovered slot
	// counts once as a completion and no longer as a failure.
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %#v, want one candidate/cohort group", reports)
	}
	if got := reports[0].Summary.Total; got != 4 {
		t.Fatalf("reported replays = %d, want the four planned slots", got)
	}
	if got := reports[0].Summary.Failures; got != 0 {
		t.Fatalf("reported failures = %d, want the retried slot counted as its completion", got)
	}
	if got := reports[0].RepeatCount; got != 2 {
		t.Fatalf("reported repeats = %d, want 2", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("flaky agent marker missing, so the first pass never took the failing branch: %v", err)
	}
}

// A local corpus recorded before replay resume existed keeps working: the
// attempt column is added in place, and every row already in it is that slot's
// first attempt, which is what the report's latest-attempt rule reads.
func TestOpenAddsTheAttemptColumnToARegistryRecordedBeforeResume(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	registry, err := sql.Open("sqlite", filepath.Join(root, "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Exec(`ALTER TABLE evaluations DROP COLUMN attempt`); err != nil {
		_ = registry.Close()
		t.Fatalf("drop the column to model a pre-resume registry: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen a pre-resume registry: %v", err)
	}
	defer reopened.Close()
	var attemptDefault string
	if err := reopened.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('evaluations') WHERE name = 'attempt'`).Scan(&attemptDefault); err != nil {
		t.Fatalf("the migration did not add the attempt column: %v", err)
	}
	if attemptDefault != "1" {
		t.Fatalf("attempt column default = %q, want every pre-resume row to be attempt 1", attemptDefault)
	}
}

// A session whose slots all completed has nothing to resume: the retry is an
// idempotent no-op that records nothing and replans nothing.
func TestReplayResumeWithNothingToRetryRecordsNothing(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, resumeFindings)

	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	session, evaluations, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluations) != 2 {
		t.Fatalf("evaluations = %d, want 2", len(evaluations))
	}

	var plan ReplayPlan
	resumed, retried, err := Replay(ctx, store, ReplayOptions{
		ResumeSessionID: session.ID,
		OnPlan:          func(p ReplayPlan) { plan = p },
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.ID != session.ID || len(retried) != 0 || len(plan.Slots) != 0 || !plan.Resumed {
		t.Fatalf("resume of a complete session = session %#v over %d replays, plan %#v", resumed, len(retried), plan)
	}
	if rows := countEvaluationRows(t, store, session.ID); rows != 2 {
		t.Fatalf("session rows = %d, want the 2 recorded slots untouched", rows)
	}
}

// A resumed slot must run as the candidate the session planned: the identity
// comes from the recorded inputs and is never re-specified by the operator, so
// a session this build cannot reproduce is refused instead of replayed as
// something else.
func TestReplayResumeRefusesASessionWhoseCandidateIdentityCannotBeReproduced(t *testing.T) {
	ctx := context.Background()
	store, _, first := storeWithOneCapturedCase(t, ctx)

	legacy := Session{
		ID:        "legacy-override-session",
		Set:       "all",
		Candidate: "claude,model=test,review_chunk_threshold_lines=501",
		Repeats:   1,
		CaseIDs:   []string{first.ID},
		Cohort:    cohortID([]string{first.ID}, 1),
	}
	sessionsDir := filepath.Join(store.root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(sessionsDir, legacy.ID+".json"), legacy); err != nil {
		t.Fatal(err)
	}

	_, _, err := Replay(ctx, store, ReplayOptions{ResumeSessionID: legacy.ID})
	if err == nil || !strings.Contains(err.Error(), "cannot reproduce") {
		t.Fatalf("resume error = %v, want the unreproducible candidate identity refused", err)
	}
	if rows := countEvaluationRows(t, store, legacy.ID); rows != 0 {
		t.Fatalf("refused resume recorded %d row(s)", rows)
	}
}

func TestReplayResumeRefusesAnUnknownSession(t *testing.T) {
	ctx := context.Background()
	store := openEvalStore(t)

	_, _, err := Replay(ctx, store, ReplayOptions{ResumeSessionID: "no-such-session"})
	if err == nil || !strings.Contains(err.Error(), "no-such-session") {
		t.Fatalf("resume error = %v, want the unknown session named", err)
	}
	if _, _, err := Replay(ctx, store, ReplayOptions{ResumeSessionID: "../escape"}); err == nil {
		t.Fatal("resume accepted a session id that is not a plain file name")
	}
}

// A replay still holding its case reservations is in flight, so resuming the
// same session would run its slots twice at once.
func TestReplayResumeRefusesASessionStillReservedByALiveReplay(t *testing.T) {
	ctx := context.Background()
	store, p, first := storeWithOneCapturedCase(t, ctx)
	installFakeReviewAgent(t, p, resumeFindings)

	session, _, err := Replay(ctx, store, ReplayOptions{Set: "all", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`,
		session.ID, first.ID, time.Now().Add(replayReservationLease).Unix()); err != nil {
		t.Fatal(err)
	}

	_, _, err = Replay(ctx, store, ReplayOptions{ResumeSessionID: session.ID})
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("resume error = %v, want a live reservation refused", err)
	}
}

// addSecondCapturedCase captures a second review pass on its own branch of the
// same source repository, so a replay session plans more than one case.
func addSecondCapturedCase(t *testing.T, ctx context.Context, p *paths.Paths, database *db.DB, repo *db.Repo, run *db.Run, store *Store) Case {
	t.Helper()
	workDir := repo.WorkingPath
	mustGit(t, ctx, workDir, "checkout", "main")
	mustGit(t, ctx, workDir, "checkout", "-b", "feature/eval-second")
	if err := os.WriteFile(filepath.Join(workDir, "second.go"), []byte("package sample\n\nfunc Second() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, ctx, workDir, "add", "second.go")
	mustGit(t, ctx, workDir, "commit", "-m", "second change")
	mustGit(t, ctx, workDir, "push", "origin", "feature/eval-second")
	headSHA := mustGit(t, ctx, workDir, "rev-parse", "HEAD")

	secondRun, err := database.InsertRun(repo.ID, "feature/eval-second", headSHA, run.BaseSHA)
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(secondRun.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	repoConfigYAML, err := os.ReadFile(filepath.Join(workDir, ".no-mistakes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	findings := resumeFindings
	round, err := database.InsertReviewStepRoundWithProvenance(step.ID, 1, "initial", &findings, nil, headSHA, headSHA, run.BaseSHA, []byte("{}\n"), repoConfigYAML, 20)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["real-bug"]`
	if err := database.SetStepRoundSelection(round.ID, &selected, db.RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}
	captured, err := Capture(ctx, store, p, database, secondRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 {
		t.Fatalf("second capture = %#v, want exactly one case", captured)
	}
	return captured[0]
}

// installFlakyReviewAgent installs a fake claude review that exits 1 with an
// empty stderr on its first invocation for the second.go case - the provider
// failure the resume mode exists to recover - and behaves from then on.
func installFlakyReviewAgent(t *testing.T, p *paths.Paths) string {
	t.Helper()
	fakeDir := t.TempDir()
	marker := filepath.Join(fakeDir, "failed-once")
	fake := filepath.Join(fakeDir, "claude")
	reply := `{"type":"assistant","message":{"usage":{"input_tokens":12,"output_tokens":3},"content":[{"type":"text","text":"review"}]}}
{"type":"result","subtype":"success","is_error":false,"structured_output":` + resumeFindings + `,"usage":{"input_tokens":12,"output_tokens":3}}
`
	var script string
	if runtime.GOOS == "windows" {
		fake += ".cmd"
		script = "@echo off\r\n" +
			"findstr /c:\"second.go\" >nul\r\n" +
			"if errorlevel 1 goto reply\r\n" +
			"if exist \"" + marker + "\" goto reply\r\n" +
			">\"" + marker + "\" echo failed\r\n" +
			"exit /b 1\r\n" +
			":reply\r\n" +
			"more >nul\r\n" +
			"echo " + strings.ReplaceAll(strings.TrimSpace(reply), "\n", "\r\necho ") + "\r\n"
	} else {
		script = "#!/bin/sh\n" +
			"prompt=$(cat)\n" +
			"case \"$prompt\" in\n" +
			"*second.go*)\n" +
			"  if [ ! -f \"" + marker + "\" ]; then : > \"" + marker + "\"; exit 1; fi\n" +
			"  ;;\n" +
			"esac\n" +
			"cat <<'EOF'\n" + reply + "EOF\n"
	}
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(fake)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

type recordedSlot struct {
	CaseID string
	Repeat int
}

func failedSlot(t *testing.T, store *Store, sessionID string) recordedSlot {
	t.Helper()
	rows, err := store.db.Query(`SELECT case_id, repeat_number FROM evaluations WHERE session_id = ? AND status = 'failed'`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var slots []recordedSlot
	for rows.Next() {
		var slot recordedSlot
		if err := rows.Scan(&slot.CaseID, &slot.Repeat); err != nil {
			t.Fatal(err)
		}
		slots = append(slots, slot)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 {
		t.Fatalf("failed slots = %#v, want exactly one for this fixture", slots)
	}
	return slots[0]
}

func readAttempt(t *testing.T, path string) Evaluation {
	t.Helper()
	var evaluation Evaluation
	if err := readJSON(path, &evaluation); err != nil {
		t.Fatalf("read recorded attempt %s: %v", filepath.Base(path), err)
	}
	return evaluation
}

func caseByID(t *testing.T, store *Store, caseID string) Case {
	t.Helper()
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.ID == caseID {
			return c
		}
	}
	t.Fatalf("case %s is not registered", caseID)
	return Case{}
}

func countEvaluationRows(t *testing.T, store *Store, sessionID string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM evaluations WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countEvaluationRowsForCase(t *testing.T, store *Store, sessionID, caseID string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT count(*) FROM evaluations WHERE session_id = ? AND case_id = ?`, sessionID, caseID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// storeWithOneCapturedCase opens a store holding exactly one captured case.
func storeWithOneCapturedCase(t *testing.T, ctx context.Context) (*Store, *paths.Paths, Case) {
	t.Helper()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	t.Cleanup(func() { _ = sourceDB.Close() })
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	captured, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 {
		t.Fatalf("captured = %#v, want one case", captured)
	}
	return store, p, captured[0]
}
