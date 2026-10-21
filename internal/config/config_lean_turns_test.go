package config

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLeanTurnsDefaultsApplyToPiOnly(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent: [pi, claude, codex]\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(cfg, &RepoConfig{})
	pi := merged.LeanTurnsFor(types.AgentPi)
	if !pi.Enabled() || !pi.Runbook || !pi.IncrementalRereview || !pi.SkipSkillCatalog {
		t.Fatalf("pi lean turns = %+v, want every default on", pi)
	}
	if pi.RunbookMaxBytes != DefaultRunbookMaxBytes || pi.FixerSessionMaxContextTokens != DefaultFixerSessionMaxContextTokens {
		t.Fatalf("pi lean bounds = %+v", pi)
	}
	for _, name := range []types.AgentName{types.AgentClaude, types.AgentCodex} {
		if got := merged.LeanTurnsFor(name); got.Enabled() || got.Runbook || got.IncrementalRereview || got.SkipSkillCatalog || got.FixerSessionMaxContextTokens != 0 {
			t.Fatalf("%s must keep its existing behavior by default, got %+v", name, got)
		}
	}
}

func TestLeanTurnsExplicitSettings(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`lean_turns:
  agents: [pi, claude]
  runbook: false
  runbook_max_bytes: 2000
  incremental_rereview: false
  fixer_session_max_context_tokens: 0
  skip_skill_catalog: false
`))
	if err != nil {
		t.Fatal(err)
	}
	merged := Merge(cfg, &RepoConfig{})
	got := merged.LeanTurnsFor(types.AgentClaude)
	if !got.Enabled() || got.Runbook || got.IncrementalRereview || got.SkipSkillCatalog || got.FixerSessionMaxContextTokens != 0 || got.RunbookMaxBytes != 2000 {
		t.Fatalf("claude lean turns = %+v", got)
	}
}

func TestLeanTurnsEmptyAgentListDisablesEverything(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("lean_turns:\n  agents: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := Merge(cfg, &RepoConfig{}).LeanTurnsFor(types.AgentPi); got.Enabled() || got.Runbook || got.IncrementalRereview {
		t.Fatalf("empty agent list must disable lean turns, got %+v", got)
	}
}

func TestLeanTurnsRejectsNegativeBounds(t *testing.T) {
	for _, body := range []string{
		"lean_turns:\n  runbook_max_bytes: -1\n",
		"lean_turns:\n  fixer_session_max_context_tokens: -5\n",
	} {
		if _, err := LoadGlobalFromBytes([]byte(body)); err == nil || !strings.Contains(err.Error(), "lean_turns") {
			t.Fatalf("%q: err = %v, want a lean_turns validation error", body, err)
		}
	}
}

// TestLeanTurnsIsNotARepoSetting proves lean_turns is inert in
// .no-mistakes.yaml: it narrows what a re-review re-reads, so a pushed branch
// must not be able to switch it on for its own review.
func TestLeanTurnsIsNotARepoSetting(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("lean_turns:\n  agents: [claude]\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := Merge(DefaultGlobalConfig(), repo).LeanTurnsFor(types.AgentClaude); got.Enabled() {
		t.Fatalf("repo config enabled lean turns for claude: %+v", got)
	}
}
