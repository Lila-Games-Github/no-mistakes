package runmetrics

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const ArtifactName = "run-metrics.v1.json"
const SummaryName = "run-metrics.txt"

type Timing struct {
	WallMS         *int64 `json:"wall_ms"`
	ExecutionMS    *int64 `json:"execution_ms"`
	ActiveMS       *int64 `json:"active_ms"`
	ExternalWaitMS *int64 `json:"external_wait_ms"`
	HumanWaitMS    *int64 `json:"human_wait_ms"`
}

type Phase struct {
	ObservedTiming   Timing `json:"observed_timing"`
	DurationRecorded bool   `json:"duration_recorded"`
	Purpose          string `json:"purpose"`
	Criticality      string `json:"criticality"`
	Outcome          string `json:"outcome"`
	ExitCode         *int   `json:"exit_code"`
	FailureClass     string `json:"failure_class"`
	Timing           Timing `json:"timing"`
	DurationSource   string `json:"duration_source"`
	ExecutionCycles  int    `json:"execution_cycles"`
	RerunCount       int    `json:"rerun_count"`
	ObservedCycleMS  int64  `json:"observed_cycle_ms"`
	FixRounds        int    `json:"fix_rounds"`
	ReportedFindings int    `json:"reported_findings"`
	ResolvedFindings int    `json:"resolved_findings"`
}

type Detail struct {
	Span
	Criticality string `json:"criticality"`
}

type Invocation struct {
	Step             string `json:"step"`
	Round            int    `json:"round"`
	Purpose          string `json:"purpose"`
	DurationMS       int64  `json:"duration_ms"`
	Outcome          string `json:"outcome"`
	FailureClass     string `json:"failure_class"`
	SessionMode      string `json:"session_mode"`
	InputTokens      *int   `json:"input_tokens"`
	CacheReadTokens  *int   `json:"cache_read_tokens"`
	FreshInputTokens *int   `json:"fresh_input_tokens"`
	SubprocessWaitMS *int64 `json:"subprocess_wait_ms"`
	ChangedFiles     *int   `json:"changed_files"`
	ChangedLines     *int   `json:"changed_lines"`
	ToolCalls        *int   `json:"tool_calls"`
	TestLintCalls    *int   `json:"test_lint_calls"`
	FindingCount     *int   `json:"finding_count"`
	ProviderQueueMS  *int64 `json:"provider_queue_ms"`
}

type Round struct {
	Step       string `json:"step"`
	Number     int    `json:"number"`
	Fix        bool   `json:"fix"`
	DurationMS int64  `json:"duration_ms"`
}

// Artifact is an explicit allowlist. Never serialize a DB row directly: rows
// also contain user text, command/config contents, paths, and error messages.
type Artifact struct {
	CollectionProblem  string       `json:"collection_problem"`
	ObservedTiming     Timing       `json:"observed_timing"`
	CollectionComplete bool         `json:"collection_complete"`
	SchemaVersion      int          `json:"schema_version"`
	RunID              string       `json:"run_id"`
	Status             string       `json:"status"`
	GeneratedAt        time.Time    `json:"generated_at"`
	Timing             Timing       `json:"timing"`
	PriorBranchRuns    int          `json:"prior_branch_runs"`
	Phases             []Phase      `json:"phases"`
	Rounds             []Round      `json:"rounds"`
	Invocations        []Invocation `json:"invocations"`
	Details            []Detail     `json:"details"`
	Health             Health       `json:"collection_health"`
	JournalPresent     bool         `json:"journal_present"`
	TornJournal        bool         `json:"torn_journal"`
}

func criticality(step string) string {
	switch step {
	case "review":
		return "risk_scaled_audit"
	case "document", "lint":
		return "supporting_validation"
	case "intent", "push", "pr":
		return "delivery_and_coordination"
	default:
		return "required_correctness"
	}
}

func detailCriticality(span Span) string {
	if span.Operation == Formatting {
		return "supporting_validation"
	}
	return criticality(span.Step)
}

func ptr[T any](v T) *T { return &v }
func value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
func add(dst **int64, ms int64) {
	if *dst == nil {
		*dst = ptr(int64(0))
	}
	**dst += ms
}

// Build reuses durable step/round/invocation evidence. Unknown legacy splits
// stay null. Detail spans overlap parent phases and must not be summed into
// the run's wall time. Step duration is the latest persisted execution, while
// observed_cycle_ms retains earlier validation passes after CI resets a step.
func Build(database *db.DB, runID, dir string, now time.Time) (*Artifact, error) {
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("run not found")
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, err
	}
	invocations, err := database.GetAgentInvocationsByRun(runID)
	if err != nil {
		return nil, err
	}
	spans, torn, err := ReadJournal(dir)
	journalProblem := ""
	if err != nil {
		journalProblem = "journal_unavailable"
	}
	info, statErr := os.Stat(filepath.Join(dir, JournalName))
	a := &Artifact{SchemaVersion: Version, RunID: run.ID, Status: string(run.Status), GeneratedAt: now.UTC(), JournalPresent: statErr == nil && info.Mode().IsRegular(), TornJournal: torn, CollectionProblem: journalProblem,
		Phases: []Phase{}, Rounds: []Round{}, Invocations: []Invocation{}, Details: []Detail{}}
	// Health is cumulative across resumed executors; missing health on an
	// active/crashed writer means completeness is not certified.
	if b, readErr := os.ReadFile(filepath.Join(dir, "metrics-health.json")); readErr == nil {
		if json.Unmarshal(b, &a.Health) != nil {
			a.TornJournal = true
		}
	}
	a.CollectionComplete = a.JournalPresent && a.CollectionProblem == "" && !a.TornJournal && a.Health.Dropped == 0 && a.Health.WriteFailures == 0 && a.Health.InterruptedSegments == 0 && a.Health.Records == len(spans) && a.Health.Records > 0 && run.Status.Terminal()
	end := now.Unix()
	if run.Status.Terminal() {
		end = run.UpdatedAt
	}
	a.Timing.ExecutionMS = ptr(int64(0))
	a.Timing.WallMS = ptr(max(int64(0), end-run.CreatedAt) * 1000)
	human := run.ParkedMS
	if run.AwaitingAgentSince != nil {
		human += max(int64(0), now.Unix()-*run.AwaitingAgentSince) * 1000
	}
	a.Timing.HumanWaitMS = ptr(human)
	for _, span := range spans {
		a.Details = append(a.Details, Detail{span, detailCriticality(span)})
		if span.Operation == Preparation {
			p := Phase{Purpose: string(Preparation), Criticality: criticality(""), Outcome: span.Outcome, FailureClass: span.FailureClass, DurationSource: "phase_metrics.target_preparation", DurationRecorded: true, ExecutionCycles: 1}
			p.Timing.ExecutionMS = ptr(span.WallMS)
			p.Timing.WallMS = ptr(span.WallMS)
			p.Timing.ActiveMS = ptr(span.WallMS)
			p.ObservedTiming = p.Timing
			a.Phases = append(a.Phases, p)
		}
	}
	for _, step := range steps {
		p := Phase{Purpose: string(step.StepName), Criticality: criticality(string(step.StepName)), Outcome: string(step.Status), ExitCode: step.ExitCode, DurationSource: "step_results.duration_ms"}
		if step.DurationMS != nil {
			p.Timing.ExecutionMS = step.DurationMS
			p.DurationRecorded = true
		}
		if step.Error != nil {
			p.FailureClass = "step_error"
		} // never copy error text
		var external, phaseHuman int64
		lastCycleFailure := ""
		for _, span := range spans {
			if span.Step != p.Purpose {
				continue
			}
			switch span.Operation {
			case Cycle:
				p.ExecutionCycles++
				p.ObservedCycleMS += span.WallMS
				lastCycleFailure = span.FailureClass
			case CIPollWait, BackoffWait:
				external += span.WallMS
			case HumanGate:
				phaseHuman += span.WallMS
			}
		}
		if step.Error != nil && lastCycleFailure != "" {
			p.FailureClass = lastCycleFailure
		}
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			return nil, err
		}
		for _, round := range rounds {
			a.Rounds = append(a.Rounds, Round{p.Purpose, round.Round, round.IsFixRound(), round.DurationMS})
			if round.IsFixRound() {
				p.FixRounds++
			}
		}
		observedCycles := p.ExecutionCycles
		if step.DurationMS != nil && *step.DurationMS > 0 && observedCycles == 0 {
			a.CollectionComplete = false
		}
		if p.ExecutionCycles == 0 {
			p.ExecutionCycles = len(rounds)
		}
		p.RerunCount = max(0, p.ExecutionCycles-1)
		stats, err := database.StepFindingStats(step)
		if err != nil {
			return nil, err
		}
		p.ReportedFindings, p.ResolvedFindings = stats.ReportedFindings, stats.FixedFindings
		if a.JournalPresent && a.CollectionProblem == "" && observedCycles > 0 {
			p.ObservedTiming.ExecutionMS = ptr(p.ObservedCycleMS)
			p.ObservedTiming.ExternalWaitMS = ptr(external)
			if a.CollectionComplete && external <= p.ObservedCycleMS {
				p.ObservedTiming.ActiveMS = ptr(p.ObservedCycleMS - external)
			}
			p.ObservedTiming.HumanWaitMS = ptr(phaseHuman)
			p.ObservedTiming.WallMS = ptr(p.ObservedCycleMS + phaseHuman)
			add(&a.ObservedTiming.ExecutionMS, p.ObservedCycleMS)
		}
		add(&a.Timing.ExecutionMS, value(p.Timing.ExecutionMS))
		a.Phases = append(a.Phases, p)
	}
	if a.JournalPresent && a.CollectionProblem == "" {
		var external int64
		for _, span := range spans {
			if span.Operation == CIPollWait || span.Operation == BackoffWait {
				external += span.WallMS
			}
		}
		a.ObservedTiming.ExternalWaitMS = ptr(external)
		if a.CollectionComplete && a.ObservedTiming.ExecutionMS != nil && external <= *a.ObservedTiming.ExecutionMS {
			a.ObservedTiming.ActiveMS = ptr(*a.ObservedTiming.ExecutionMS - external)
		}
		var observedHuman int64
		for _, span := range spans {
			if span.Operation == HumanGate {
				observedHuman += span.WallMS
			}
		}
		a.ObservedTiming.HumanWaitMS = ptr(observedHuman)
		a.ObservedTiming.WallMS = ptr(value(a.ObservedTiming.ExecutionMS) + observedHuman)
	}
	if !a.CollectionComplete {
		for i := range a.Phases {
			a.Phases[i].ObservedTiming.ActiveMS = nil
		}
	}
	for _, inv := range invocations {
		a.Invocations = append(a.Invocations, Invocation{Step: inv.StepName, Round: inv.Round, Purpose: inv.Purpose, DurationMS: inv.DurationMS,
			Outcome: inv.ExitStatus, FailureClass: inv.FailureCategory, SessionMode: inv.SessionMode, InputTokens: inv.DeltaInputTokens,
			CacheReadTokens: inv.DeltaCacheReadTokens, FreshInputTokens: inv.FreshInputTokens, SubprocessWaitMS: inv.SubprocessWaitMS,
			ChangedFiles: inv.WorkloadFiles, ChangedLines: inv.WorkloadLines, ToolCalls: inv.ToolCalls, TestLintCalls: inv.ToolTestLintCalls, FindingCount: inv.FindingCount})
	}
	prior, err := database.GetRunsByRepo(run.RepoID)
	if err != nil {
		return nil, err
	}
	for _, other := range prior {
		if other.Branch == run.Branch && other.ID < run.ID {
			a.PriorBranchRuns++
		}
	}
	return a, nil
}

func Summary(a *Artifact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s (%s), metrics v%d\n", a.RunID, a.Status, a.SchemaVersion)
	fmt.Fprintf(&b, "Wall %.1fs; persisted step execution %.1fs; human gates %.1fs\n", float64(*a.Timing.WallMS)/1000, float64(value(a.Timing.ExecutionMS))/1000, float64(*a.Timing.HumanWaitMS)/1000)
	if a.ObservedTiming.ExternalWaitMS != nil {
		fmt.Fprintf(&b, "Observed external waits %.1fs", float64(*a.ObservedTiming.ExternalWaitMS)/1000)
		if a.ObservedTiming.ActiveMS == nil {
			fmt.Fprintln(&b, "; active execution split unknown")
		} else {
			fmt.Fprintf(&b, "; observed cycle execution excluding waits %.1fs\n", float64(*a.ObservedTiming.ActiveMS)/1000)
		}
	}
	phases := []Phase{}
	for _, phase := range a.Phases {
		if phase.DurationRecorded {
			phases = append(phases, phase)
		}
	}
	sort.SliceStable(phases, func(i, j int) bool { return value(phases[i].Timing.ExecutionMS) > value(phases[j].Timing.ExecutionMS) })
	fmt.Fprintln(&b, "Longest phases (latest persisted step execution; setup from journal):")
	for _, phase := range phases[:min(5, len(phases))] {
		fmt.Fprintf(&b, "  %s %.1fs [%s, %s]\n", phase.Purpose, float64(value(phase.Timing.ExecutionMS))/1000, phase.Criticality, phase.Outcome)
	}
	for _, phase := range a.Phases {
		if phase.RerunCount > 0 {
			fmt.Fprintf(&b, "Repeated work: %s %d cycles, %d fix rounds", phase.Purpose, phase.ExecutionCycles, phase.FixRounds)
			if a.JournalPresent {
				fmt.Fprintf(&b, "; observed cycles %.1fs\n", float64(phase.ObservedCycleMS)/1000)
			} else {
				fmt.Fprintln(&b, "; observed cycle timing unknown")
			}
		}
	}
	var resumed, fallback, commands, coverage int
	for _, inv := range a.Invocations {
		if inv.SessionMode == db.InvocationModeResumed {
			resumed++
		}
		if inv.SessionMode == db.InvocationModeFallback {
			fallback++
		}
		if inv.TestLintCalls != nil {
			commands += *inv.TestLintCalls
			coverage++
		}
	}
	fmt.Fprintf(&b, "Agent attempts: %d (%d resumed, %d fallback); test/lint tool calls: %d in %d observed attempts\n", len(a.Invocations), resumed, fallback, commands, coverage)
	for _, phase := range a.Phases {
		if phase.Purpose == "review" {
			fmt.Fprintf(&b, "Review value: %d reported, %d no longer present in final findings (not proof of correctness)\n", phase.ReportedFindings, phase.ResolvedFindings)
		}
	}
	fmt.Fprintln(&b, "Criticality versus duration:")
	for _, tag := range []string{"required_correctness", "risk_scaled_audit", "supporting_validation", "delivery_and_coordination"} {
		var ms int64
		for _, phase := range a.Phases {
			if phase.Criticality == tag {
				ms += value(phase.Timing.ExecutionMS)
			}
		}
		fmt.Fprintf(&b, "  %s %.1fs\n", tag, float64(ms)/1000)
	}
	fmt.Fprintf(&b, "Coverage: journal=%t, torn=%t, dropped=%d, write failures=%d. Detail spans overlap; null means unknown.\n", a.JournalPresent, a.TornJournal, a.Health.Dropped, a.Health.WriteFailures)
	fmt.Fprintf(&b, "Collection sealed and complete: %t; observed coverage may be partial when false.\n", a.CollectionComplete)
	if a.CollectionProblem != "" {
		fmt.Fprintf(&b, "Collection problem: %s\n", a.CollectionProblem)
	}
	fmt.Fprintln(&b, "Provider queues and agent-internal format/build timing are unknown unless separately observed. Tags never waive gates.")
	return b.String()
}

// Finish runs during cleanup. Telemetry failures are bounded diagnostics and
// never replace the pipeline's result. Atomic replacement prevents torn JSON.
func Finish(r *Recorder, database *db.DB, runID, dir string) {
	health := r.Close()
	b, _ := json.Marshal(health)
	_ = atomicWrite(dir, "metrics-health.json", b)
	if err := Publish(database, runID, dir); err != nil {
		slog.Warn("local run metrics unavailable", "reason", "snapshot_write_failed")
	}
}

// Publish regenerates a snapshot after terminal crash recovery as well as
// normal writer cleanup. Callers keep this outside validation decisions.
func Publish(database *db.DB, runID, dir string) error {
	a, err := Build(database, runID, dir, time.Now())
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(dir, ArtifactName, append(b, '\n')); err != nil {
		return err
	}
	return atomicWrite(dir, SummaryName, []byte(Summary(a)))
}

func atomicWrite(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".metrics-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = f.Chmod(0o644)
	if err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
