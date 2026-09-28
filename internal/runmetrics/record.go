// Package runmetrics records local, content-free pipeline performance evidence.
// It never controls a gate or sends records to remote analytics.
package runmetrics

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const Version = 1
const JournalName = "phase-metrics.jsonl"

type Operation string

const (
	Preparation Operation = "target_preparation"
	Worktree    Operation = "worktree_preparation"
	Fetch       Operation = "fetch"
	Rebase      Operation = "rebase"
	Diff        Operation = "diff"
	Context     Operation = "context_construction"
	Agent       Operation = "agent_execution"
	Command     Operation = "validation_command"
	Formatting  Operation = "formatting"
	BackoffWait Operation = "agent_retry_wait"
	Cycle       Operation = "execution_cycle"
	CIPollWait  Operation = "ci_poll_wait"
	HumanGate   Operation = "human_gate"
)

// Input is deliberately numeric. Prompts, commands, paths and error text have
// no representation in the journal. Unknown values are null, not zero.
type Input struct {
	DispatchQueueMS *int64 `json:"dispatch_queue_ms"`
	ChangedFiles    *int   `json:"changed_files"`
	ChangedLines    *int   `json:"changed_lines"`
	ContextBytes    *int   `json:"context_bytes"`
}

type Span struct {
	SchemaVersion int       `json:"schema_version"`
	Step          string    `json:"step"`
	Round         int       `json:"round"`
	Operation     Operation `json:"operation"`
	StartedMS     int64     `json:"started_ms"`
	WallMS        int64     `json:"wall_ms"`
	Outcome       string    `json:"outcome"`
	FailureClass  string    `json:"failure_class"`
	ExitCode      *int      `json:"exit_code"`
	Input         Input     `json:"input"`
}

type Health struct {
	InterruptedSegments int `json:"interrupted_segments"`
	Records             int `json:"records"`
	Dropped             int `json:"dropped_records"`
	WriteFailures       int `json:"write_failures"`
}

type Recorder struct {
	mu     sync.Mutex
	queue  chan Span
	done   chan struct{}
	closed bool
	health Health
}

type scope struct {
	recorder    *Recorder
	step        string
	round       int
	preparation *contextPreparation
}
type contextPreparation struct {
	once   sync.Once
	finish func(error, *int, Input)
}
type contextKey struct{}

// Attach starts a bounded, nonblocking writer. A context already attached to
// this run retains its writer when setup hands ownership to the executor.
func Attach(ctx context.Context, dir string) (context.Context, *Recorder) {
	if s, ok := ctx.Value(contextKey{}).(scope); ok {
		return ctx, s.recorder
	}
	r := &Recorder{queue: make(chan Span, 256), done: make(chan struct{})}
	go r.write(dir)
	return context.WithValue(ctx, contextKey{}, scope{recorder: r}), r
}

// AttachTo transfers only the recorder to a fresh background context, never
// the request's cancellation, credentials or other context values.
func AttachTo(ctx, from context.Context) (context.Context, *Recorder) {
	s, ok := from.Value(contextKey{}).(scope)
	if !ok {
		return ctx, nil
	}
	return context.WithValue(ctx, contextKey{}, scope{recorder: s.recorder}), s.recorder
}

// BeginContext observes local setup through the first agent dispatch of a
// cycle. This includes diff/history loading, so it overlaps those subspans.
func BeginContext(ctx context.Context) context.Context {
	s, ok := ctx.Value(contextKey{}).(scope)
	if !ok {
		return ctx
	}
	s.preparation = &contextPreparation{finish: Start(ctx, Context)}
	return context.WithValue(ctx, contextKey{}, s)
}

func ContextReady(ctx context.Context, bytes int) {
	if s, ok := ctx.Value(contextKey{}).(scope); ok && s.preparation != nil {
		s.preparation.once.Do(func() { s.preparation.finish(nil, nil, Input{ContextBytes: &bytes}) })
	}
}

func Scope(ctx context.Context, step string, round int) context.Context {
	s, ok := ctx.Value(contextKey{}).(scope)
	if !ok {
		return ctx
	}
	if !validStep(step) {
		step = "unknown"
	}
	s.step, s.round = step, max(0, round)
	return context.WithValue(ctx, contextKey{}, s)
}

func validStep(step string) bool {
	switch step {
	case "", "intent", "rebase", "review", "test", "document", "lint", "push", "pr", "ci", "unknown":
		return true
	}
	return false
}

func validOperation(op Operation) bool {
	switch op {
	case Preparation, Worktree, Fetch, Rebase, Diff, Context, Agent, Command, Formatting, BackoffWait, Cycle, CIPollWait, HumanGate:
		return true
	}
	return false
}

// Start returns a completion callback. Timing is monotonic; wall timestamps
// are only for correlation. Nested spans are details, never additive totals.
func Start(ctx context.Context, op Operation) func(error, *int, Input) {
	s, ok := ctx.Value(contextKey{}).(scope)
	if !ok {
		return func(error, *int, Input) {}
	}
	if !validOperation(op) {
		return func(error, *int, Input) {}
	}
	start := time.Now()
	return func(err error, exit *int, input Input) {
		outcome, failure := "ok", ""
		if err != nil || (exit != nil && *exit != 0) {
			outcome, failure = "error", "other"
			switch {
			case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
				failure = "timeout"
			case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
				outcome, failure = "cancelled", "cancelled"
			case exit != nil && *exit != 0:
				failure = "exit"
			default:
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					failure = "exit"
					code := ee.ExitCode()
					exit = &code
				}
			}
		}
		s.recorder.record(Span{Version, s.step, s.round, op, start.UnixMilli(), time.Since(start).Milliseconds(), outcome, failure, exit, input})
	}
}

func (r *Recorder) record(span Span) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	select {
	case r.queue <- span:
	default:
		r.health.Dropped++
	}
}

// Close drains the writer only at ownership cleanup, outside gate execution.
func (r *Recorder) Close() Health {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.health
}

func (r *Recorder) write(dir string) {
	defer close(r.done)
	var prior Health
	healthBytes, healthErr := os.ReadFile(filepath.Join(dir, "metrics-health.json"))
	if healthErr == nil {
		_ = json.Unmarshal(healthBytes, &prior)
	}
	r.mu.Lock()
	r.health.Records += prior.Records
	r.health.Dropped += prior.Dropped
	r.health.WriteFailures += prior.WriteFailures
	r.health.InterruptedSegments += prior.InterruptedSegments
	r.mu.Unlock()
	var file *os.File
	if os.MkdirAll(dir, 0o755) == nil {
		_ = os.Remove(filepath.Join(dir, "metrics-health.json"))
		file, _ = os.OpenFile(filepath.Join(dir, JournalName), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o644)
	}
	if file != nil {
		defer file.Close()
		// A crash may leave a partial line. Keep completed records and remove
		// only the unterminated tail before appending a new segment.
		if info, err := file.Stat(); err == nil && info.Size() > 0 {
			if healthErr != nil {
				r.mu.Lock()
				r.health.InterruptedSegments++
				r.mu.Unlock()
			}
			if err := repairTail(file, info.Size()); err != nil {
				r.mu.Lock()
				r.health.WriteFailures++
				r.mu.Unlock()
			}
		}
	}
	for span := range r.queue {
		r.mu.Lock()
		r.health.Records++
		r.mu.Unlock()
		if file == nil || json.NewEncoder(file).Encode(span) != nil {
			r.mu.Lock()
			r.health.WriteFailures++
			r.mu.Unlock()
		}
	}
}

func repairTail(file *os.File, size int64) error {
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	// Records are small fixed schemas; scanning chunks also handles a corrupt
	// oversized tail without allocating its full size.
	var chunk [4096]byte
	for end := size; end > 0; {
		start := max(int64(0), end-int64(len(chunk)))
		buf := chunk[:end-start]
		if _, err := file.ReadAt(buf, start); err != nil {
			return err
		}
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return file.Truncate(start + int64(i) + 1)
			}
		}
		end = start
	}
	return file.Truncate(0)
}

// ReadJournal tolerates a torn final line after a crash. Other corruption is
// surfaced, so a report cannot present partial evidence as complete.
func ReadJournal(dir string) ([]Span, bool, error) {
	f, err := os.Open(filepath.Join(dir, JournalName))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var spans []Span
	torn := false
	for scanner.Scan() {
		if torn {
			return spans, true, errors.New("corrupt metrics journal")
		}
		var span Span
		if json.Unmarshal(scanner.Bytes(), &span) != nil {
			torn = true
			continue
		}
		if span.SchemaVersion != Version || !validStep(span.Step) || !validOperation(span.Operation) || span.WallMS < 0 || span.Round < 0 {
			return spans, true, errors.New("invalid metrics record")
		}
		spans = append(spans, span)
	}
	return spans, torn, scanner.Err()
}
