package pipeline

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	defaultRunInputBudget    = 1_500_000
	defaultReviewInputBudget = 150_000
)

// ErrAgentTimeout is the context cause used when the default per-invocation
// agent deadline expires. Callers wrap it with a diagnostic that names the
// budget; a late successful return after this cause is still a timeout.
var ErrAgentTimeout = errors.New("agent timeout")

// AgentTimeout is the per-invocation budget applied at the shared agent-run
// seam. A positive Config.AgentTimeout wins; otherwise the default (30m).
func AgentTimeout(cfg *config.Config) time.Duration {
	if cfg != nil && cfg.AgentTimeout > 0 {
		return cfg.AgentTimeout
	}
	return config.DefaultAgentTimeout
}

// RunAgent executes one agent invocation with a deadline scoped only to that
// call. The parent StepContext.Ctx is left unchanged so post-agent work
// (commits, git, parsing) is not cancelled by the invocation budget.
//
// If the parent context already has a deadline (review's round budget, Test's
// explicit wrap, intent extraction, caller cancellation), that bound is
// honored and no shorter default is stacked. Otherwise AgentTimeout is
// applied. A late successful return after the deadline is rejected.
func (sctx *StepContext) RunAgent(opts agent.RunOpts) (*agent.Result, error) {
	parent := context.Background()
	if sctx != nil {
		parent = sctx.Ctx
	}
	return sctx.runAgent(parent, opts, "")
}

// RunAgentContext is RunAgent with an explicit parent, used when a step has
// already installed a more specific deadline (review round, Test invocation).
func (sctx *StepContext) RunAgentContext(parent context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, "")
}

// RunAgentSessionContext is RunAgentSession with an explicit parent so a
// fixer turn can share a round budget (review) or a per-invocation wrap (Test).
func (sctx *StepContext) RunAgentSessionContext(parent context.Context, role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	return sctx.runAgent(parent, opts, role)
}

func (sctx *StepContext) runAgent(parent context.Context, opts agent.RunOpts, sessionRole SessionRole) (*agent.Result, error) {
	var ag agent.Agent
	timeout := AgentTimeout(nil)
	if sctx != nil {
		ag = sctx.Agent
		timeout = AgentTimeout(sctx.Config)
		brief, err := sctx.preparedContextBrief(parent)
		if err != nil {
			return nil, err
		}
		if brief != "" {
			opts.PreparedContext = true
			log, err := sctx.contextRunningLog()
			if err != nil {
				return nil, err
			}
			opts.Prompt = brief + "\n---\nRunning log (outcomes and decisions only):\n" + log + "\nCurrent pipeline step:\n" + opts.Prompt
		}
		if sctx.Config != nil && sctx.Config.ContextBrief && sctx.DB != nil && sctx.Run != nil {
			spent, err := sctx.recordedRunInputTokens()
			if err != nil {
				return nil, err
			}
			limit := defaultRunInputBudget - spent
			if limit <= 0 {
				return nil, fmt.Errorf("%w: run consumed %d of %d input tokens", agent.ErrInputTokenBudget, spent, defaultRunInputBudget)
			}
			// The actual provider may differ from the wrapper's primary name.
			// Carry Claude's policy through dispatch and refresh the allowance
			// after any failed provider or fixer resume, whose usage is recorded.
			if sctx.StepName == types.StepReview || sctx.StepName == types.StepCI {
				opts.ClaudeWorkBudget = &agent.BoundedWorkBudget{
					MaxInputTokens: defaultReviewInputBudget,
					RemainingInputTokens: func() (int, error) {
						spent, err := sctx.recordedRunInputTokens()
						return defaultRunInputBudget - spent, err
					},
				}
			}
			if opts.BoundedWork {
				opts.MaxTotalInputTokens = limit
			}
			if (sctx.StepName == types.StepReview || opts.BoundedWork) && limit > defaultReviewInputBudget {
				limit = defaultReviewInputBudget
			}
			if opts.MaxInputTokens == 0 || opts.MaxInputTokens > limit {
				opts.MaxInputTokens = limit
			}
		}
	}
	result, err := invokeAgent(parent, timeout, func(ctx context.Context) (*agent.Result, error) {
		if sessionRole != "" && sctx != nil && sctx.Sessions != nil {
			return sctx.Sessions.Run(ctx, ag, sessionRole, opts, sctx.Log)
		}
		if ag == nil {
			return nil, errors.New("nil agent")
		}
		return ag.Run(ctx, opts)
	})
	if err != nil {
		return nil, err
	}
	if opts.MaxInputTokens > 0 && result != nil && result.UsageReported {
		provider := sessionProvider(ag, result)
		limit := opts.MaxInputTokens
		bounded := opts.BoundedWork
		if provider == "claude" && opts.ClaudeWorkBudget != nil {
			bounded = true
			limit = min(limit, opts.ClaudeWorkBudget.MaxInputTokens)
		}
		used := invocationInputTokens(provider, result.Usage)
		if bounded && result.PeakContextTokens > 0 {
			used = result.PeakContextTokens
		}
		if used > limit {
			return nil, fmt.Errorf("%w: call consumed %d of %d input tokens", agent.ErrInputTokenBudget, used, limit)
		}
	}
	return result, nil
}

func invocationInputTokens(name string, usage agent.TokenUsage) int {
	if name == "claude" {
		return usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	}
	return usage.InputTokens + usage.CacheCreationTokens
}

func (sctx *StepContext) recordedRunInputTokens() (int, error) {
	rows, err := sctx.DB.GetAgentInvocationsByRun(sctx.Run.ID)
	if err != nil {
		return 0, fmt.Errorf("read run input-token usage: %w", err)
	}
	total := 0
	for _, row := range rows {
		if row.Agent == "claude" {
			total += perRoundInput(row) + row.CacheReadTokens
		} else {
			total += perRoundInput(row)
		}
		if row.CacheCreationTokens != nil {
			total += *row.CacheCreationTokens
		}
	}
	return total, nil
}

func perRoundInput(row db.AgentInvocation) int {
	if row.DeltaInputTokens != nil {
		return *row.DeltaInputTokens
	}
	return row.InputTokens
}

func invokeAgent(parent context.Context, timeout time.Duration, run func(context.Context) (*agent.Result, error)) (*agent.Result, error) {
	ctx, cancel, applied := bindAgentDeadline(parent, timeout)
	result, err := run(ctx)
	runErr := classifyAgentRun(ctx, applied, err)
	cancel()
	if runErr != nil {
		return nil, runErr
	}
	return result, nil
}

func bindAgentDeadline(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return parent, func() {}, 0
	}
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}, 0
	}
	ctx, cancel := context.WithTimeoutCause(parent, timeout, ErrAgentTimeout)
	return ctx, cancel, timeout
}

func classifyAgentRun(ctx context.Context, applied time.Duration, err error) error {
	if applied > 0 && errors.Is(context.Cause(ctx), ErrAgentTimeout) {
		return fmt.Errorf("agent timed out after %s (agent silent for %s): %w", applied, applied, ErrAgentTimeout)
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err != nil {
		return err
	}
	return nil
}

// timeoutAgent is the executor backstop: every sctx.Agent.Run is bounded even
// if a future step forgets RunAgent. Nested with RunAgent it is a no-op when
// the incoming context already has a deadline.
type timeoutAgent struct {
	inner   agent.Agent
	timeout time.Duration
}

func (a *timeoutAgent) Name() string { return a.inner.Name() }

func (a *timeoutAgent) Close() error { return a.inner.Close() }

func (a *timeoutAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return invokeAgent(ctx, a.timeout, func(runCtx context.Context) (*agent.Result, error) {
		return a.inner.Run(runCtx, opts)
	})
}

func (a *timeoutAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *timeoutAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *timeoutAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *timeoutAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}
