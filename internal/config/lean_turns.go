package config

import (
	"fmt"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	// DefaultRunbookMaxBytes caps the repository run-book injected into each
	// review and review-fix prompt.
	DefaultRunbookMaxBytes = 6000
	// DefaultFixerSessionMaxContextTokens is the per-call context size past
	// which the review fixer's durable session is retired and the next fix
	// round starts fresh. The prompt already carries the round history, so the
	// fresh session loses only transcript, never the decisions.
	DefaultFixerSessionMaxContextTokens = 200000
)

// LeanTurnsRaw is the YAML representation of lean_turns. Pointer fields
// distinguish "not set" (nil) from explicit zero/false values.
type LeanTurnsRaw struct {
	Agents                       *[]types.AgentName `yaml:"agents"`
	Runbook                      *bool              `yaml:"runbook"`
	RunbookMaxBytes              *int               `yaml:"runbook_max_bytes"`
	IncrementalRereview          *bool              `yaml:"incremental_rereview"`
	FixerSessionMaxContextTokens *int               `yaml:"fixer_session_max_context_tokens"`
	SkipSkillCatalog             *bool              `yaml:"skip_skill_catalog"`
}

// LeanTurns is the resolved lean_turns setting: the turn-economy measures
// applied to the listed agents only. It is global-only: it describes how this
// machine's harnesses are driven, and incremental re-review narrows what a
// re-review reads, so no pushed branch may switch it on for its own review.
// The default list is pi, the harness these measures were sized against, so
// every other agent keeps its exact previous behavior unless the operator
// lists it.
type LeanTurns struct {
	Agents []types.AgentName
	// Runbook feeds the cached repository run-book (internal/runbook) to
	// review, review-fix, test, document, and lint agent turns.
	Runbook         bool
	RunbookMaxBytes int
	// IncrementalRereview scopes a post-fix re-review to the fix-round delta
	// plus the findings that prompted it. A full-scope review still runs before
	// the review step can complete.
	IncrementalRereview bool
	// FixerSessionMaxContextTokens retires the durable fixer session once one
	// of its model calls carried more context than this. 0 never retires it.
	FixerSessionMaxContextTokens int
	// SkipSkillCatalog launches harnesses that inject a skills catalog into
	// every system prompt (pi) without it.
	SkipSkillCatalog bool
}

// Enabled reports whether this resolved value applies to an agent.
func (l LeanTurns) Enabled() bool { return len(l.Agents) > 0 }

func leanTurnsDefaults() LeanTurns {
	return LeanTurns{
		Agents:                       []types.AgentName{types.AgentPi},
		Runbook:                      true,
		RunbookMaxBytes:              DefaultRunbookMaxBytes,
		IncrementalRereview:          true,
		FixerSessionMaxContextTokens: DefaultFixerSessionMaxContextTokens,
		SkipSkillCatalog:             true,
	}
}

func applyLeanTurnsOverrides(dst *LeanTurns, src *LeanTurnsRaw) {
	if src.Agents != nil {
		dst.Agents = copyAgents(*src.Agents)
	}
	if src.Runbook != nil {
		dst.Runbook = *src.Runbook
	}
	if src.RunbookMaxBytes != nil {
		dst.RunbookMaxBytes = *src.RunbookMaxBytes
	}
	if src.IncrementalRereview != nil {
		dst.IncrementalRereview = *src.IncrementalRereview
	}
	if src.FixerSessionMaxContextTokens != nil {
		dst.FixerSessionMaxContextTokens = *src.FixerSessionMaxContextTokens
	}
	if src.SkipSkillCatalog != nil {
		dst.SkipSkillCatalog = *src.SkipSkillCatalog
	}
}

func validateLeanTurnsRaw(raw LeanTurnsRaw) error {
	if raw.RunbookMaxBytes != nil && *raw.RunbookMaxBytes < 0 {
		return fmt.Errorf("lean_turns.runbook_max_bytes must be 0 (no cap) or greater, got %d", *raw.RunbookMaxBytes)
	}
	if raw.FixerSessionMaxContextTokens != nil && *raw.FixerSessionMaxContextTokens < 0 {
		return fmt.Errorf("lean_turns.fixer_session_max_context_tokens must be 0 (never retire) or greater, got %d", *raw.FixerSessionMaxContextTokens)
	}
	return nil
}

// LeanTurnsFor returns the lean_turns measures that apply to one agent, or
// the zero value (every measure off) when the agent is not listed.
func (c *Config) LeanTurnsFor(name types.AgentName) LeanTurns {
	if c == nil || !slices.Contains(c.LeanTurns.Agents, name) {
		return LeanTurns{}
	}
	out := c.LeanTurns
	out.Agents = []types.AgentName{name}
	return out
}
