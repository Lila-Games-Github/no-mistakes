package runmetrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func fixture(t *testing.T) (*db.DB, *db.Run, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepo("/home/private/check-out", "https://private:secret@example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "secret-branch-name", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	return d, run, dir
}

func TestArtifactReusesDurableEvidenceAndDoesNotPublishContent(t *testing.T) {
	d, run, dir := fixture(t)
	review, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := `{"findings":[{"id":"f1","severity":"blocking","description":"SENSITIVE-SOURCE","file":"/home/private/source.go"}]}`
	if _, err := d.InsertStepRound(review.ID, 1, "initial", &sensitive, nil, 2000); err != nil {
		t.Fatal(err)
	}
	clean := `{"findings":[]}`
	if _, err := d.InsertStepRound(review.ID, 2, "auto_fix", &clean, ptr("SENSITIVE-SUMMARY"), 1000); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(review.ID, 0, 3000, "/home/private/log"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddRunParkedDuration(run.ID, 500); err != nil {
		t.Fatal(err)
	}
	_, err = d.InsertAgentInvocation(db.AgentInvocation{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", Model: "private-model", SessionKey: "private-session", SessionMode: db.InvocationModeResumed, DurationMS: 1000, ExitStatus: "ok", DeltaInputTokens: ptr(100), DeltaCacheReadTokens: ptr(80), FreshInputTokens: ptr(20), WorkloadFiles: ptr(2), WorkloadLines: ptr(40), FindingCount: ptr(0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	ctx, r := Attach(context.Background(), dir)
	r.record(Span{SchemaVersion: Version, Step: "review", Operation: Cycle, WallMS: 2000})
	r.record(Span{SchemaVersion: Version, Step: "review", Operation: Cycle, WallMS: 1000})
	r.record(Span{SchemaVersion: Version, Step: "review", Operation: BackoffWait, WallMS: 250})
	r.record(Span{SchemaVersion: Version, Step: "review", Operation: HumanGate, WallMS: 500})
	Start(ctx, Operation("SENSITIVE-OPERATION"))(errors.New("SENSITIVE-ERROR"), nil, Input{})
	Finish(r, d, run.ID, dir)
	b, err := os.ReadFile(filepath.Join(dir, ArtifactName))
	if err != nil {
		t.Fatal(err)
	}
	var a Artifact
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if a.SchemaVersion != 1 || !a.CollectionComplete || len(a.Phases) != 1 || len(a.Invocations) != 1 {
		t.Fatalf("artifact: %+v", a)
	}
	p := a.Phases[0]
	if *p.Timing.ExecutionMS != 3000 || *p.ObservedTiming.ExecutionMS != 3000 || *p.ObservedTiming.ActiveMS != 2750 || *p.ObservedTiming.ExternalWaitMS != 250 || *p.ObservedTiming.HumanWaitMS != 500 {
		t.Fatalf("timing: %+v", p)
	}
	if p.RerunCount != 1 || p.FixRounds != 1 || p.ReportedFindings != 1 || p.ResolvedFindings != 1 {
		t.Fatalf("review value: %+v", p)
	}
	if *a.Invocations[0].CacheReadTokens != 80 || *a.Invocations[0].ChangedFiles != 2 || a.Invocations[0].ProviderQueueMS != nil {
		t.Fatalf("input shape: %+v", a.Invocations[0])
	}
	summary, err := os.ReadFile(filepath.Join(dir, SummaryName))
	if err != nil {
		t.Fatal(err)
	}
	all := string(b) + string(summary)
	for _, forbidden := range []string{"SENSITIVE", "/home/private", "private-session", "private-model", "secret-branch", "example.com"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("artifact exposed %q", forbidden)
		}
	}
	for _, want := range []string{"Repeated work: review 2 cycles", "Review value: 1 reported", "risk_scaled_audit 3.0s", "Tags never waive gates"} {
		if !strings.Contains(string(summary), want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	inv := decoded["invocations"].([]any)[0].(map[string]any)
	if v, present := inv["provider_queue_ms"]; !present || v != nil {
		t.Fatalf("unknown queue must be explicit null: %v", inv)
	}
}

func TestLegacyProofBaselineLeavesUnobservedSplitsUnknown(t *testing.T) {
	d, run, dir := fixture(t)
	durations := []struct {
		step types.StepName
		ms   int64
	}{
		{types.StepIntent, 14}, {types.StepRebase, 1430}, {types.StepReview, 22605}, {types.StepTest, 60636}, {types.StepDocument, 17284}, {types.StepPush, 3519}, {types.StepPR, 16042}, {types.StepCI, 70308},
	}
	for _, sample := range durations {
		s, err := d.InsertStepResult(run.ID, sample.step)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteStep(s.ID, 0, sample.ms, ""); err != nil {
			t.Fatal(err)
		}
	}
	a, err := Build(d, run.ID, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if *a.Timing.ExecutionMS != 191838 || a.JournalPresent || a.CollectionComplete || a.ObservedTiming.ExternalWaitMS != nil {
		t.Fatalf("legacy totals: %+v", a)
	}
	for _, p := range a.Phases {
		if p.ObservedTiming.ActiveMS != nil || p.ObservedTiming.HumanWaitMS != nil || p.Timing.WallMS != nil {
			t.Fatalf("fabricated legacy split: %+v", p)
		}
	}
	summary := Summary(a)
	if strings.Index(summary, "ci 70.3s") > strings.Index(summary, "test 60.6s") {
		t.Fatal("longest phases must be sorted")
	}
}

func TestRepeatedValidationRetainsEarlierCycleDuration(t *testing.T) {
	d, run, dir := fixture(t)
	s, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(s.ID, 0, 2000, ""); err != nil {
		t.Fatal(err)
	}
	_, r := Attach(context.Background(), dir)
	r.record(Span{SchemaVersion: Version, Step: "ci", Operation: Cycle, WallMS: 10000})
	r.record(Span{SchemaVersion: Version, Step: "ci", Operation: CIPollWait, WallMS: 9000})
	r.record(Span{SchemaVersion: Version, Step: "ci", Operation: Cycle, WallMS: 2000})
	r.record(Span{SchemaVersion: Version, Step: "ci", Operation: CIPollWait, WallMS: 1000})
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	Finish(r, d, run.ID, dir)
	a, err := Build(d, run.ID, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p := a.Phases[0]
	if *p.Timing.ExecutionMS != 2000 || p.ObservedCycleMS != 12000 || *p.ObservedTiming.ActiveMS != 2000 || *p.ObservedTiming.ExternalWaitMS != 10000 {
		t.Fatalf("mixed duration bases: %+v", p)
	}
}

func TestRecorderConcurrentScopeAndContentFreeFailures(t *testing.T) {
	dir := t.TempDir()
	ctx, r := Attach(context.Background(), dir)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := Scope(ctx, "review", i+1)
			code := 3
			Start(c, Command)(errors.New("secret command failure"), &code, Input{ChangedFiles: ptr(2)})
		}(i)
	}
	wg.Wait()
	health := r.Close()
	spans, torn, err := ReadJournal(dir)
	if err != nil || torn || len(spans) != 20 || health.Dropped != 0 || health.WriteFailures != 0 {
		t.Fatalf("recording: %+v, spans=%d torn=%t err=%v", health, len(spans), torn, err)
	}
	for _, span := range spans {
		if span.Step != "review" || span.FailureClass != "exit" || *span.ExitCode != 3 || span.WallMS < 0 {
			t.Fatalf("span: %+v", span)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, JournalName))
	if strings.Contains(string(b), "secret") {
		t.Fatal("error prose leaked")
	}
}

func TestRecorderFailureAndQueueSaturationNeverFailWork(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, r := Attach(context.Background(), file)
	Start(ctx, Fetch)(nil, nil, Input{})
	if health := r.Close(); health.WriteFailures != 1 {
		t.Fatalf("health: %+v", health)
	}
	// A stalled writer cannot block the invocation; excess records are counted.
	stalled := &Recorder{queue: make(chan Span, 1)}
	stalled.record(Span{})
	stalled.record(Span{})
	if stalled.health.Dropped != 1 {
		t.Fatal("queue saturation not accounted for")
	}
}

func TestRecorderResumesAfterTornJournal(t *testing.T) {
	dir := t.TempDir()
	ctx, first := Attach(context.Background(), dir)
	Start(Scope(ctx, "review", 1), Cycle)(nil, nil, Input{})
	first.Close() // simulate a crash before the health seal is published
	f, err := os.OpenFile(filepath.Join(dir, JournalName), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema_version":`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, torn, err := ReadJournal(dir)
	if err != nil || !torn {
		t.Fatalf("expected torn tail: %t %v", torn, err)
	}
	ctx, second := Attach(context.Background(), dir)
	Start(Scope(ctx, "review", 2), Cycle)(nil, nil, Input{})
	health := second.Close()
	spans, torn, err := ReadJournal(dir)
	if err != nil || torn || len(spans) != 2 || health.InterruptedSegments != 1 {
		t.Fatalf("resume: health=%+v spans=%d torn=%t err=%v", health, len(spans), torn, err)
	}
}

func TestUnsupportedJournalVersionFallsBackToDatabaseEvidence(t *testing.T) {
	d, run, dir := fixture(t)
	if err := os.WriteFile(filepath.Join(dir, JournalName), []byte(`{"schema_version":2,"step":"review","operation":"execution_cycle","wall_ms":3}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := Build(d, run.ID, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.CollectionProblem != "journal_unavailable" || a.CollectionComplete || len(a.Details) != 0 || a.ObservedTiming.ActiveMS != nil {
		t.Fatalf("unsupported schema accepted: %+v", a)
	}
}

func TestPartialJournalDoesNotInventActiveOrEarlierPhaseTiming(t *testing.T) {
	d, run, dir := fixture(t)
	review, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStep(review.ID, 0, 22605, ""); err != nil {
		t.Fatal(err)
	}
	ci, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	ctx, r := Attach(context.Background(), dir)
	Start(Scope(ctx, "ci", 1), CIPollWait)(nil, nil, Input{})
	r.Close() // no completed CI cycle or seal yet
	a, err := Build(d, run.ID, dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.CollectionComplete || a.ObservedTiming.ActiveMS != nil || a.Phases[0].ObservedTiming.ExecutionMS != nil || a.Phases[1].Timing.ExecutionMS != nil {
		t.Fatalf("invented partial timing: %+v", a)
	}
	if !strings.Contains(Summary(a), "active execution split unknown") {
		t.Fatal("partial coverage not explained")
	}
}
