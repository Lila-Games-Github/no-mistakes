package eval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReplayOptions controls one isolated candidate comparison. The optional
// callbacks observe progress for interactive rendering: OnPlan fires once the
// case set is reserved and the session is recorded, OnResult after each
// replay's evaluation is persisted. Both run synchronously on the replay
// goroutine and may be nil.
type ReplayOptions struct {
	Set       string
	Candidate Candidate
	Repeats   int
	// ResumeSessionID replays only the failed or missing slots of an existing
	// recorded session. Set, Candidate, Repeats, CaseIDs and the isolated
	// overrides are then read from that session and never from this struct, so
	// a resumed slot always runs as the candidate and cohort it was planned
	// for. Each retry is recorded as a new attempt beside the failed row.
	ResumeSessionID string
	// CaseIDs narrows the selected set to exact captured cases for paired
	// experiments. Both variants then retain the same cohort identity.
	CaseIDs []string
	// ReviewChunkThresholdLines overrides the captured review setting only in
	// this isolated replay. Nil preserves the captured configuration. Zero is
	// the unchunked control; positive values must keep medium changes intact.
	ReviewChunkThresholdLines *int
	// ReviewChunkMaxLines overrides the captured per-turn changed-line budget
	// of a chunked review in this isolated replay. Nil keeps the captured
	// value; it is part of the candidate identity.
	ReviewChunkMaxLines *int
	// LeanTurns overrides whether the candidate agent gets the lean_turns
	// measures in this isolated replay: true lists the candidate agent (with
	// the captured or default bounds), false lists no agent. Nil preserves the
	// captured configuration. The override is part of the candidate identity.
	LeanTurns *bool
	OnPlan    func(plan ReplayPlan)
	OnResult  func(evaluation Evaluation, completed, total int)
}

// Session records the immutable local plan used for one replay batch.
type Session struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Set       string    `json:"set"`
	Candidate string    `json:"candidate"`
	Repeats   int       `json:"repeats"`
	CaseIDs   []string  `json:"case_ids"`
	Cohort    string    `json:"cohort"`
	// CandidateSpec is the operator's candidate spelling and the three
	// isolated overrides are its other inputs, recorded so a resume can
	// re-derive the exact identity this session planned under instead of
	// trusting a flag (see Session.resumeCandidate). A session recorded before
	// resume existed leaves them empty.
	CandidateSpec             string `json:"candidate_spec,omitempty"`
	ReviewChunkThresholdLines *int   `json:"review_chunk_threshold_lines,omitempty"`
	ReviewChunkMaxLines       *int   `json:"review_chunk_max_lines,omitempty"`
	LeanTurns                 *bool  `json:"lean_turns,omitempty"`
}

// ReplayPlan is the slot-level work one replay batch executes: one entry per
// (case, repeat) it will invoke the candidate on.
type ReplayPlan struct {
	Session Session
	Slots   []ReplaySlot
	// Total is how many slots the session plans in full, so a resume can
	// report how much of it is being retried.
	Total int
	// Resumed reports that this plan retries only the failed or missing slots
	// of an existing session rather than planning every slot from scratch.
	Resumed bool
}

// ReplaySlot is one planned unit of replay work. Attempt is the try number the
// slot is recorded as; a slot is identified in every user-facing surface by
// its case ID, repeat, and attempt.
type ReplaySlot struct {
	CaseID  string
	Repeat  int
	Attempt int
	// caseRecord is the captured case the slot restores.
	caseRecord Case
}

const (
	replayReservationLease   = 5 * time.Minute
	replayReservationRefresh = time.Minute
)

// Replay runs exactly the captured review pass. It does not start a daemon or
// use the production NM_HOME: every case is restored into a fresh temp gate and
// worktree. Push, PR, CI, and all fix loops are intentionally absent from the
// MVP subject under test.
//
// With ReplayOptions.ResumeSessionID it instead replans the failed or missing
// slots of that recorded session; see resumeReplay.
func Replay(ctx context.Context, store *Store, opts ReplayOptions) (Session, []Evaluation, error) {
	if store == nil {
		return Session{}, nil, fmt.Errorf("eval replay requires a store")
	}
	if opts.ResumeSessionID != "" {
		return store.resumeReplay(ctx, opts)
	}
	if opts.Repeats <= 0 {
		return Session{}, nil, fmt.Errorf("repeats must be at least 1")
	}
	if err := opts.Candidate.Validate(); err != nil {
		return Session{}, nil, err
	}
	if opts.ReviewChunkThresholdLines != nil && *opts.ReviewChunkThresholdLines != 0 && *opts.ReviewChunkThresholdLines <= 500 {
		return Session{}, nil, fmt.Errorf("review chunk threshold must be 0 or greater than 500")
	}
	if opts.ReviewChunkMaxLines != nil && *opts.ReviewChunkMaxLines != 0 && *opts.ReviewChunkMaxLines < config.MinReviewChunkMaxLines {
		return Session{}, nil, fmt.Errorf("review chunk max lines must be 0 or at least %d", config.MinReviewChunkMaxLines)
	}
	cases, session, err := store.prepareReplay(ctx, opts)
	if err != nil {
		return Session{}, nil, err
	}
	replayCtx, cancelReplay := context.WithCancel(ctx)
	stopReservation := store.keepReplayReservation(replayCtx, cancelReplay, session.ID)

	plan := ReplayPlan{Session: session, Total: len(cases) * opts.Repeats}
	// Repeat-major, matching the order the slots are invoked in.
	for repeat := 1; repeat <= opts.Repeats; repeat++ {
		for _, c := range cases {
			plan.Slots = append(plan.Slots, ReplaySlot{CaseID: c.ID, Repeat: repeat, Attempt: 1, caseRecord: c})
		}
	}
	return store.runReplayPlan(replayCtx, cancelReplay, stopReservation, plan, opts.Candidate, replayOverrides{chunkThreshold: opts.ReviewChunkThresholdLines, chunkMaxLines: opts.ReviewChunkMaxLines, leanTurns: opts.LeanTurns}, opts.OnPlan, opts.OnResult)
}

// runReplayPlan invokes the candidate on every planned slot under the
// session's reservation lease and records one evaluation per slot. A failed
// invocation is recorded, never skipped, and reported at the end. It owns the
// lease it is handed: the caller must have created a cancellable context that
// cancels the run when the reservation can no longer be refreshed.
func (s *Store) runReplayPlan(ctx context.Context, cancelReplay context.CancelFunc, stopReservation func(), plan ReplayPlan, candidate Candidate, overrides replayOverrides, onPlan func(ReplayPlan), onResult func(Evaluation, int, int)) (Session, []Evaluation, error) {
	session := plan.Session
	defer func() {
		cancelReplay()
		stopReservation()
		s.releaseReplayReservation(session.ID)
	}()
	if onPlan != nil {
		onPlan(plan)
	}
	total := len(plan.Slots)
	evaluations := make([]Evaluation, 0, total)
	var failed int
	for _, slot := range plan.Slots {
		evaluation := replayOne(ctx, s, slot, session, candidate, overrides)
		if evaluation.Status != "completed" {
			failed++
		}
		if err := s.persistEvaluation(slot.caseRecord, evaluation); err != nil {
			return session, evaluations, err
		}
		evaluations = append(evaluations, evaluation)
		if onResult != nil {
			onResult(evaluation, len(evaluations), total)
		}
	}
	if failed > 0 {
		return session, evaluations, fmt.Errorf("%d replay invocation(s) failed; inspect eval report for the local failure count", failed)
	}
	return session, evaluations, nil
}

// resumeReplay replans an existing session's failed or missing slots. The
// session is the authority for everything that makes the samples comparable -
// its case set, repeat count, cohort, and candidate identity - so a resume
// cannot be pointed at a different comparison, and every retry lands beside
// the failed row as its own attempt instead of overwriting it.
func (s *Store) resumeReplay(ctx context.Context, opts ReplayOptions) (Session, []Evaluation, error) {
	plan, candidate, overrides, err := s.resumePlan(ctx, opts.ResumeSessionID)
	if err != nil {
		return Session{}, nil, err
	}
	replayCtx, cancelReplay := context.WithCancel(ctx)
	stopReservation := s.keepReplayReservation(replayCtx, cancelReplay, plan.Session.ID)
	return s.runReplayPlan(replayCtx, cancelReplay, stopReservation, plan, candidate, overrides, opts.OnPlan, opts.OnResult)
}

// resumePlan selects the slots a resume will retry and reserves the session's
// cases. It holds the corpus lock only for that planning window, never for the
// replay itself, so a multi-hour retry cannot block a capture or a prune.
func (s *Store) resumePlan(ctx context.Context, sessionID string) (ReplayPlan, Candidate, replayOverrides, error) {
	unlock, err := lockCorpus(ctx, s.root)
	if err != nil {
		return ReplayPlan{}, Candidate{}, replayOverrides{}, err
	}
	defer unlock()

	session, err := s.loadSession(sessionID)
	if err != nil {
		return ReplayPlan{}, Candidate{}, replayOverrides{}, err
	}
	candidate, overrides, err := session.resumeCandidate()
	if err != nil {
		return ReplayPlan{}, Candidate{}, replayOverrides{}, err
	}
	cases, err := s.sessionCases(session)
	if err != nil {
		return ReplayPlan{}, Candidate{}, replayOverrides{}, err
	}
	plan := ReplayPlan{Session: session, Total: len(cases) * session.Repeats, Resumed: true}
	for repeat := 1; repeat <= session.Repeats; repeat++ {
		for _, c := range cases {
			attempt, status, err := s.latestSlotAttempt(session, c, repeat)
			if err != nil {
				return ReplayPlan{}, Candidate{}, replayOverrides{}, err
			}
			if status == "completed" {
				continue
			}
			plan.Slots = append(plan.Slots, ReplaySlot{CaseID: c.ID, Repeat: repeat, Attempt: attempt + 1, caseRecord: c})
		}
	}
	if err := s.reserveSessionCases(ctx, session, cases); err != nil {
		return ReplayPlan{}, Candidate{}, replayOverrides{}, err
	}
	return plan, candidate, overrides, nil
}

// loadSession reads one recorded session. The id is a file name this tool
// generated, so anything else is refused rather than resolved.
func (s *Store) loadSession(id string) (Session, error) {
	sessionsDir := filepath.Join(s.root, "sessions")
	if strings.TrimSpace(id) == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return Session{}, fmt.Errorf("eval session %q is not a recorded session id", id)
	}
	var session Session
	path := filepath.Join(sessionsDir, id+".json")
	if err := readJSON(path, &session); err != nil {
		if os.IsNotExist(err) {
			return Session{}, fmt.Errorf("eval session %q is not recorded under %s", id, sessionsDir)
		}
		return Session{}, fmt.Errorf("read eval session %q: %w", id, err)
	}
	if session.Repeats <= 0 || len(session.CaseIDs) == 0 {
		return Session{}, fmt.Errorf("eval session %q records no replay plan", id)
	}
	return session, nil
}

// resumeCandidate reproduces the candidate identity the session was planned
// with, from the inputs it recorded. The identity is never re-specified by the
// operator: it is re-derived and must match the recorded spelling exactly, so a
// resumed slot can never be attributed to a candidate its cohort never
// compared. A session this build cannot reproduce - one recorded before the
// inputs were persisted, under an agent name that has since been removed - is
// refused rather than replayed as something else.
func (s Session) resumeCandidate() (Candidate, replayOverrides, error) {
	spec := s.CandidateSpec
	if spec == "" {
		// Sessions written before the inputs were recorded carry only their
		// derived identity. It is usable when it parses as a plain candidate,
		// which is exactly when its identity has no isolated override.
		spec = s.Candidate
	}
	candidate, err := ParseCandidate(spec)
	if err != nil {
		return Candidate{}, replayOverrides{}, fmt.Errorf("eval session %s cannot reproduce candidate %q: %w; rerun the case set instead of resuming it", s.ID, s.Candidate, err)
	}
	overrides := replayOverrides{chunkThreshold: s.ReviewChunkThresholdLines, chunkMaxLines: s.ReviewChunkMaxLines, leanTurns: s.LeanTurns}
	if derived := candidateIdentity(candidate, overrides); derived != s.Candidate {
		return Candidate{}, replayOverrides{}, fmt.Errorf("eval session %s cannot reproduce candidate %q (its recorded inputs derive %q); rerun the case set instead of resuming it", s.ID, s.Candidate, derived)
	}
	return candidate, overrides, nil
}

// sessionCases resolves the recorded case IDs to their captured records,
// independently of the case set they were selected from: the session's own
// case list is the comparison it was planned for, and a case that has since
// left its set (a relabel, a diversified re-pin) still belongs to it.
func (s *Store) sessionCases(session Session) ([]Case, error) {
	all, err := s.ListCases("all")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]Case, len(all))
	for _, c := range all {
		byID[c.ID] = c
	}
	cases := make([]Case, 0, len(session.CaseIDs))
	for _, id := range session.CaseIDs {
		c, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("eval session %s planned case %s, which is no longer in the local corpus", session.ID, id)
		}
		cases = append(cases, c)
	}
	return cases, nil
}

// latestSlotAttempt reports the highest attempt recorded for one slot and its
// status. No row means the slot was never replayed.
func (s *Store) latestSlotAttempt(session Session, c Case, repeat int) (int, string, error) {
	var attempt int
	var status string
	err := s.db.QueryRow(`SELECT attempt, status FROM evaluations WHERE session_id = ? AND case_id = ? AND candidate = ? AND repeat_number = ? ORDER BY attempt DESC LIMIT 1`,
		session.ID, c.ID, session.Candidate, repeat).Scan(&attempt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("read recorded eval slot: %w", err)
	}
	return attempt, status, nil
}

// reserveSessionCases re-reserves a resumed session's cases for as long as the
// retry runs, so a corpus prune cannot reclaim a case mid-replay (see
// Store.Prune). A reservation still held by a live replay means the same
// session is being replayed right now, which a second resume would duplicate.
func (s *Store) reserveSessionCases(ctx context.Context, session Session, cases []Case) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin eval replay reservation: %w", err)
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM replay_case_reservations WHERE session_id = ? AND reserved_until > ?`, session.ID, time.Now().Unix()).Scan(&live); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("inspect eval replay reservations: %w", err)
	}
	if live > 0 {
		_ = tx.Rollback()
		return fmt.Errorf("eval session %s is still reserved by a replay in flight; wait for it to finish before resuming it", session.ID)
	}
	reservedUntil := time.Now().Add(replayReservationLease).Unix()
	for _, c := range cases {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`, session.ID, c.ID, reservedUntil); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("reserve eval case %q: %w", c.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit eval replay reservation: %w", err)
	}
	return nil
}

// candidateIdentity is the recorded identity of one candidate: its spelling
// plus the isolated overrides that take part in it. Two variants of the same
// agent never collapse into one reported group.
func candidateIdentity(candidate Candidate, overrides replayOverrides) string {
	name := candidate.String()
	if overrides.chunkThreshold != nil {
		name = fmt.Sprintf("%s,review_chunk_threshold_lines=%d", name, *overrides.chunkThreshold)
	}
	if overrides.chunkMaxLines != nil {
		name = fmt.Sprintf("%s,review_chunk_max_lines=%d", name, *overrides.chunkMaxLines)
	}
	if overrides.leanTurns != nil {
		state := "off"
		if *overrides.leanTurns {
			state = "on"
		}
		name = fmt.Sprintf("%s,lean_turns=%s", name, state)
	}
	return name
}

func (s *Store) prepareReplay(ctx context.Context, opts ReplayOptions) ([]Case, Session, error) {
	unlock, err := lockCorpus(ctx, s.root)
	if err != nil {
		return nil, Session{}, err
	}
	defer unlock()

	cases, err := s.ListCases(opts.Set)
	if err != nil {
		return nil, Session{}, err
	}
	if len(opts.CaseIDs) > 0 {
		wanted := make(map[string]bool, len(opts.CaseIDs))
		for _, id := range opts.CaseIDs {
			wanted[id] = true
		}
		filtered := cases[:0]
		for _, c := range cases {
			if wanted[c.ID] {
				filtered = append(filtered, c)
				delete(wanted, c.ID)
			}
		}
		if len(wanted) > 0 {
			return nil, Session{}, fmt.Errorf("requested eval case is absent from set %q", opts.Set)
		}
		cases = filtered
	}
	if len(cases) == 0 {
		return nil, Session{}, fmt.Errorf("case set %q is empty", opts.Set)
	}
	session := Session{
		ID:        newSessionID(),
		StartedAt: time.Now().UTC(),
		Set:       opts.Set,
		Candidate: candidateIdentity(opts.Candidate, replayOverrides{chunkThreshold: opts.ReviewChunkThresholdLines, chunkMaxLines: opts.ReviewChunkMaxLines, leanTurns: opts.LeanTurns}),
		// The inputs are recorded beside the identity they derive, so a
		// resume reproduces it instead of trusting a flag (resumeCandidate).
		CandidateSpec:             opts.Candidate.String(),
		Repeats:                   opts.Repeats,
		ReviewChunkThresholdLines: opts.ReviewChunkThresholdLines,
		ReviewChunkMaxLines:       opts.ReviewChunkMaxLines,
		LeanTurns:                 opts.LeanTurns,
	}
	for _, c := range cases {
		session.CaseIDs = append(session.CaseIDs, c.ID)
	}
	session.Cohort = cohortID(session.CaseIDs, session.Repeats)
	sessionsDir := filepath.Join(s.root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, Session{}, fmt.Errorf("create eval sessions directory: %w", err)
	}
	sessionPath := filepath.Join(sessionsDir, session.ID+".json")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, Session{}, fmt.Errorf("begin eval replay reservation: %w", err)
	}
	reservedUntil := time.Now().Add(replayReservationLease).Unix()
	for _, c := range cases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`, session.ID, c.ID, reservedUntil); err != nil {
			_ = tx.Rollback()
			return nil, Session{}, fmt.Errorf("reserve eval case %q: %w", c.ID, err)
		}
	}
	if err := writeJSON(sessionPath, session); err != nil {
		_ = tx.Rollback()
		return nil, Session{}, fmt.Errorf("write eval session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = os.Remove(sessionPath)
		return nil, Session{}, fmt.Errorf("commit eval replay reservation: %w", err)
	}
	return cases, session, nil
}

func (s *Store) keepReplayReservation(ctx context.Context, cancelReplay context.CancelFunc, sessionID string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(replayReservationRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				reservedUntil := time.Now().Add(replayReservationLease).Unix()
				if _, err := s.db.ExecContext(ctx, `UPDATE replay_case_reservations SET reserved_until = ? WHERE session_id = ?`, reservedUntil, sessionID); err != nil {
					cancelReplay()
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (s *Store) releaseReplayReservation(sessionID string) {
	_, _ = s.db.Exec(`DELETE FROM replay_case_reservations WHERE session_id = ?`, sessionID)
}

// replayOverrides are the isolated-replay configuration overrides a paired
// experiment varies; nil fields keep the captured configuration.
type replayOverrides struct {
	chunkThreshold *int
	chunkMaxLines  *int
	leanTurns      *bool
}

func replayOne(ctx context.Context, store *Store, slot ReplaySlot, session Session, candidate Candidate, overrides replayOverrides) (evaluation Evaluation) {
	c := slot.caseRecord
	started := time.Now()
	evaluation = Evaluation{
		ID:        newSessionID(),
		SessionID: session.ID,
		CaseID:    c.ID,
		Candidate: session.Candidate,
		Cohort:    session.Cohort,
		Repeat:    slot.Repeat,
		Attempt:   slot.Attempt,
		StartedAt: started.Unix(),
		Status:    "failed",
	}
	evaluation.HasFindingGold = c.Labels.HasGold()
	evaluation.GoldCount = c.Labels.TrueIssueCount()
	evaluation.FalsePositiveGold = c.Labels.FalsePositiveCount()
	defer func() {
		if evaluation.Status != "completed" {
			evaluation.FalseNegative = evaluation.GoldCount
			if evaluation.CompletedAt == 0 {
				evaluation.CompletedAt = time.Now().Unix()
			}
		}
	}()

	// The replay sandbox deliberately stays OUTSIDE the source NM_HOME: it
	// materializes its own nested NM_HOME and worktree, and the eval store,
	// object pools, and Store.Prune all live under <NM_HOME>/eval, so a live
	// sandbox nested there would sit inside the very state it is replaying.
	// That isolation outranks moving it off the system temp directory; see the
	// held-scope note in AGENTS.md ("no-mistakes Owns Its Own Scratch").
	root, err := os.MkdirTemp("", "nm-eval-replay-")
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated replay root: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer os.RemoveAll(root)

	isolatedPaths := paths.WithRoot(filepath.Join(root, "nmhome"))
	if err := isolatedPaths.EnsureDirs(); err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated eval state: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	isolatedHome := filepath.Join(root, "home")
	if err := os.MkdirAll(isolatedHome, 0o755); err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated eval home: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	ownership, err := e2edaemon.Acquire(isolatedPaths.Root(), "", 2*time.Minute)
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("acquire isolated eval ownership: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer ownership.Release()

	workDir, err := restoreCase(ctx, store, c, root)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	cfg, err := replayConfig(c)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	if overrides.chunkThreshold != nil {
		cfg.Review.ChunkThresholdLines = *overrides.chunkThreshold
	}
	if overrides.chunkMaxLines != nil {
		cfg.Review.ChunkMaxLines = *overrides.chunkMaxLines
	}
	if overrides.leanTurns != nil {
		cfg.LeanTurns.Agents = nil
		if *overrides.leanTurns {
			cfg.LeanTurns.Agents = []types.AgentName{candidate.Agent}
		}
	}
	cfg.Agent = candidate.Agent
	cfg.Agents = []types.AgentName{candidate.Agent}

	// The candidate's tuning goes through the same harness-neutral Profile the
	// pipeline uses, so eval and a real run reach each harness's model and
	// effort mechanism by exactly one code path. Raw args stay empty: capture
	// strips agent_args_override and agent_config from the pinned config so a
	// replay cannot inherit the capturing machine's own pins.
	baseAgent, err := agent.NewWithOptions(candidate.Agent, cfg.AgentPathFor(candidate.Agent), nil, agent.Options{
		ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
		DisableProjectSettings: cfg.DisableProjectSettings,
		Profile:                candidate.Profile(),
		SkipSkillCatalog:       cfg.LeanTurnsFor(candidate.Agent).SkipSkillCatalog,
	})
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create candidate agent: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer baseAgent.Close()
	observed := &observedAgent{inner: agent.WithSteering(baseAgent, isolatedPaths.EvidenceDir()), ownership: ownership}

	replayDB, stepResultID, fixing, previousFindings, err := replayRoundContext(isolatedPaths, c, workDir)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer replayDB.Close()

	startingHeadSHA := c.StartingHeadSHA
	if startingHeadSHA == "" {
		startingHeadSHA = c.ReviewedHeadSHA
	}
	replayRun := &db.Run{ID: "eval-" + evaluation.ID, Branch: c.Branch, HeadSHA: c.ReviewedHeadSHA, BaseSHA: c.BaseSHA}
	if c.Intent != "" {
		intent := c.Intent
		replayRun.Intent = &intent
	}
	if c.IntentSource != "" {
		source := c.IntentSource
		replayRun.IntentSource = &source
	}
	defaultBranch := c.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	replayRepo := &db.Repo{ID: "eval", WorkingPath: workDir, DefaultBranch: defaultBranch}
	step := &steps.ReviewStep{}
	outcome, err := step.Execute(&pipeline.StepContext{
		Ctx:                   ctx,
		Run:                   replayRun,
		Repo:                  replayRepo,
		WorkDir:               workDir,
		Agent:                 observed,
		Config:                cfg,
		DB:                    replayDB,
		StepResultID:          stepResultID,
		Fixing:                fixing,
		SkipFixExecution:      fixing,
		ReviewStartingHeadSHA: startingHeadSHA,
		PreviousFindings:      previousFindings,
		Env:                   replayEnv(isolatedPaths.Root(), isolatedHome),
		Log:                   func(string) {},
		LogChunk:              func(string) {},
		LogFile:               func(string) {},
		UserIntent:            c.Intent,
		IntentSource:          c.IntentSource,
	})
	// Candidate wall time is the actual review invocation, matching the local
	// agent-invocation metric rather than charging case restoration setup.
	evaluation.DurationMS = observed.durationMS
	if evaluation.DurationMS == 0 && observed.result == nil {
		evaluation.DurationMS = time.Since(started).Milliseconds()
	}
	evaluation.CompletedAt = time.Now().Unix()
	if observed.result != nil {
		if observed.modelMismatch {
			evaluation.Error = "candidate served different models across review chunks"
			return evaluation
		}
		evaluation.Model = observed.model
		if evaluation.Model == "" {
			evaluation.Model = candidate.Model
		} else if evaluation.Model != candidate.Model {
			evaluation.Error = safeurl.RedactText(fmt.Sprintf("candidate served model %q, requested %q", evaluation.Model, candidate.Model))
			return evaluation
		}
		if !observed.usageMissing {
			evaluation.TokensReported = true
			evaluation.InputTokens = int64(observed.usage.InputTokens)
			evaluation.OutputTokens = int64(observed.usage.OutputTokens)
			evaluation.CacheReadTokens = int64(observed.usage.CacheReadTokens)
			evaluation.FreshInputTokens = int64(agent.FreshInputTokens(observed.usage.InputTokens, observed.usage.CacheReadTokens))
		}
	}
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("replay review: %v", err))
		return evaluation
	}
	if outcome == nil {
		evaluation.Error = "replay review returned no outcome"
		return evaluation
	}
	evaluation.Status = "completed"
	evaluation.FindingsJSON = outcome.Findings
	evaluation.FindingCount = findingCount(outcome.Findings)
	score := ScoreCandidate(c.Labels, outcome.Findings)
	evaluation.TruePositive = score.TruePositive
	evaluation.TruePositiveExact = score.TruePositiveExact
	evaluation.TruePositiveFuzzy = score.TruePositiveFuzzy
	evaluation.FalseNegative = score.FalseNegative
	evaluation.FalsePositive = score.FalsePositive
	evaluation.FalsePositiveGold = score.FalsePositiveGold
	evaluation.Pending = score.Pending
	return evaluation
}

// harnessConfigDirs are the agent harnesses' own configuration directories
// (credentials, settings) that normally live under HOME, keyed by the
// variable each harness reads to relocate them.
var harnessConfigDirs = []struct{ env, rel string }{
	{"CLAUDE_CONFIG_DIR", ".claude"},
	{"CODEX_HOME", ".codex"},
	{"PI_CODING_AGENT_DIR", filepath.Join(".pi", "agent")},
}

// replayEnv isolates HOME and NM_HOME for the replayed review while keeping
// each harness's own configuration reachable. Without the pass-through a
// harness that stores its login under HOME runs signed out: claude exits 1
// with an empty stderr and pi reports no authenticated provider, so the
// replay measures nothing. A harness variable the operator already set is
// inherited unchanged, and a directory that does not exist is not invented.
func replayEnv(nmHome, isolatedHome string) []string {
	env := []string{"NM_HOME=" + nmHome, "HOME=" + isolatedHome}
	realHome, err := os.UserHomeDir()
	if err != nil || realHome == "" {
		return env
	}
	for _, dir := range harnessConfigDirs {
		if os.Getenv(dir.env) != "" {
			continue
		}
		path := filepath.Join(realHome, dir.rel)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			env = append(env, dir.env+"="+path)
		}
	}
	return env
}

func replayRoundContext(p *paths.Paths, c Case, workDir string) (*db.DB, string, bool, string, error) {
	var selected sourceRound
	if err := readJSON(filepath.Join(c.Dir, "original", "round.json"), &selected); err != nil {
		return nil, "", false, "", fmt.Errorf("read captured review round: %w", err)
	}
	var rounds []sourceRound
	if err := readJSON(filepath.Join(c.Dir, "original", "rounds.json"), &rounds); err != nil {
		return nil, "", false, "", fmt.Errorf("read captured round history: %w", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return nil, "", false, "", fmt.Errorf("open isolated replay database: %w", err)
	}
	fail := func(err error) (*db.DB, string, bool, string, error) {
		database.Close()
		return nil, "", false, "", err
	}
	repo, err := database.InsertRepoWithID("eval-repo", workDir, "local://eval", c.DefaultBranch)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay repository: %w", err))
	}
	run, err := database.InsertRun(repo.ID, c.Branch, c.ReviewedHeadSHA, c.BaseSHA)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay run: %w", err))
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay step: %w", err))
	}
	var previousFindings string
	for _, round := range rounds {
		if round.StepName != "" && round.StepName != string(types.StepReview) {
			continue
		}
		if round.Round >= selected.Round {
			continue
		}
		inserted, err := database.InsertReviewStepRound(step.ID, round.Round, round.Trigger, round.FindingsJSON, round.FixSummary, stringValue(round.ReviewedHeadSHA), round.DurationMS)
		if err != nil {
			return fail(fmt.Errorf("restore captured review history: %w", err))
		}
		if round.UserFindingsJSON != nil {
			if err := database.SetStepRoundUserFindings(inserted.ID, round.UserFindingsJSON); err != nil {
				return fail(fmt.Errorf("restore captured user findings: %w", err))
			}
		}
		if round.SelectedFindingIDs != nil {
			source := stringValue(round.SelectionSource)
			if err := database.SetStepRoundSelection(inserted.ID, round.SelectedFindingIDs, source); err != nil {
				return fail(fmt.Errorf("restore captured gate decision: %w", err))
			}
		}
		if round.Round == selected.Round-1 {
			previousFindings = stringValue(round.UserFindingsJSON)
			if previousFindings == "" {
				previousFindings = stringValue(round.FindingsJSON)
			}
		}
	}
	fixing := selected.Trigger == "auto_fix" || selected.Trigger == "user_fix"
	if fixing && previousFindings == "" {
		return fail(fmt.Errorf("captured fix round %d has no previous findings", selected.Round))
	}
	return database, step.ID, fixing, previousFindings, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func restoreCase(ctx context.Context, store *Store, c Case, root string) (string, error) {
	gateDir := filepath.Join(root, "gate.git")
	if err := git.InitBare(ctx, gateDir); err != nil {
		return "", fmt.Errorf("create isolated eval gate: %w", err)
	}
	if err := restoreCaseObjects(ctx, store.poolDir(c.RepoFingerprint), gateDir, c.ID); err != nil {
		return "", err
	}
	defaultBranch := c.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "refs/remotes/origin/"+defaultBranch, c.TrustedConfigSHA); err != nil {
		return "", fmt.Errorf("restore trusted default branch: %w", err)
	}
	workDir := filepath.Join(root, "worktree")
	if err := git.WorktreeAdd(ctx, gateDir, workDir, c.ReviewedHeadSHA); err != nil {
		return "", fmt.Errorf("restore review worktree: %w", err)
	}
	return workDir, nil
}

func replayConfig(c Case) (*config.Config, error) {
	global, err := config.LoadGlobal(filepath.Join(c.Dir, "config", "global.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load captured global config: %w", err)
	}
	repoBytes, err := os.ReadFile(filepath.Join(c.Dir, "config", "repo-config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read captured repo config: %w", err)
	}
	repo, err := config.LoadRepoFromBytes(repoBytes)
	if err != nil {
		return nil, fmt.Errorf("load captured repo config: %w", err)
	}
	return config.Merge(global, repo), nil
}

type observedAgent struct {
	inner         agent.Agent
	ownership     *e2edaemon.Ownership
	result        *agent.Result
	durationMS    int64
	usage         agent.TokenUsage
	usageMissing  bool
	model         string
	modelMismatch bool
	ownershipErr  error
	mu            sync.Mutex
}

func (a *observedAgent) Name() string { return a.inner.Name() }
func (a *observedAgent) Close() error { return a.inner.Close() }
func (a *observedAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	previousLifecycle := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		if event.Phase == "start" && event.PID > 0 {
			if syncErr := a.ownership.SyncProcess(event.PID); syncErr != nil {
				a.mu.Lock()
				if a.ownershipErr == nil {
					a.ownershipErr = syncErr
				}
				a.mu.Unlock()
			}
		}
		if previousLifecycle != nil {
			previousLifecycle(event)
		}
	}
	started := time.Now()
	result, err := a.inner.Run(ctx, opts)
	a.durationMS += time.Since(started).Milliseconds()
	a.result = result
	if result == nil || !result.UsageReported {
		a.usageMissing = true
	} else {
		a.usage.InputTokens += result.Usage.InputTokens
		a.usage.OutputTokens += result.Usage.OutputTokens
		a.usage.CacheReadTokens += result.Usage.CacheReadTokens
	}
	if result != nil && result.Model != "" {
		if a.model != "" && a.model != result.Model {
			a.modelMismatch = true
		}
		if a.model == "" {
			a.model = result.Model
		}
	}
	a.mu.Lock()
	ownershipErr := a.ownershipErr
	a.mu.Unlock()
	if ownershipErr != nil {
		return result, ownershipErr
	}
	return result, err
}

func findingCount(raw string) int {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return 0
	}
	return len(findings.Items)
}

func (s *Store) persistEvaluation(c Case, evaluation Evaluation) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("eval registry is closed")
	}
	candidateDir := candidatePathPart(evaluation.Candidate)
	resultDir := filepath.Join(c.Dir, "evals", evaluation.SessionID, candidateDir)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return fmt.Errorf("create eval result directory: %w", err)
	}
	// A retry is a new attempt and never overwrites the row it retries.
	path := filepath.Join(resultDir, attemptFileName(evaluation.Repeat, evaluation.Attempt))
	if err := writeJSON(path, evaluation); err != nil {
		return fmt.Errorf("write eval result: %w", err)
	}
	reported := 0
	if evaluation.TokensReported {
		reported = 1
	}
	_, err := s.db.Exec(`INSERT INTO evaluations
(id, session_id, case_id, candidate, repeat_number, attempt, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evaluation.ID, evaluation.SessionID, evaluation.CaseID, evaluation.Candidate, evaluation.Repeat, evaluation.Attempt,
		evaluation.StartedAt, evaluation.CompletedAt, evaluation.Status, evaluation.GoldCount,
		evaluation.TruePositive, evaluation.FalseNegative, evaluation.FalsePositive, evaluation.Pending, reported,
		evaluation.InputTokens, evaluation.OutputTokens, evaluation.FreshInputTokens, evaluation.DurationMS, path)
	if err != nil {
		return fmt.Errorf("record eval result: %w", err)
	}
	return nil
}

// attemptFileName is the file name one attempt is persisted under inside its
// case's session directory. The first attempt keeps the name a session has
// always used; a retry names its attempt beside it.
func attemptFileName(repeat, attempt int) string {
	if attempt <= 1 {
		return fmt.Sprintf("repeat-%03d.json", repeat)
	}
	return fmt.Sprintf("repeat-%03d-attempt-%02d.json", repeat, attempt)
}

func candidatePathPart(candidate string) string {
	var b strings.Builder
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	if b.Len() == 0 {
		return "candidate"
	}
	return b.String()
}

func cohortID(caseIDs []string, repeats int) string {
	ids := append([]string(nil), caseIDs...)
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", repeats, strings.Join(ids, "\x00"))))
	return hex.EncodeToString(sum[:8])
}

func newSessionID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(bytes[:]))
}
