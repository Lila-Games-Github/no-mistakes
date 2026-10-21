package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runbook"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// leanTurnsFor resolves the lean_turns measures for the step's agent. With a
// fallback list the primary agent decides: the prompts it shapes stay valid
// for a fallback harness, which simply receives the leaner prompt.
func leanTurnsFor(sctx *pipeline.StepContext) config.LeanTurns {
	if sctx == nil || sctx.Config == nil || sctx.Agent == nil {
		return config.LeanTurns{}
	}
	return sctx.Config.LeanTurnsFor(types.AgentName(sctx.Agent.Name()))
}

// runbookPromptSection renders the cached repository run-book for lean
// review and review-fix turns. It is read from the trusted default-branch
// commit the run's trusted config came from (falling back to the branch
// base, which is already on the target branch), never from the pushed head.
// Any failure omits the section: the run-book is an accelerator, not a gate.
func runbookPromptSection(sctx *pipeline.StepContext, lean config.LeanTurns, baseSHA string) string {
	if !lean.Runbook {
		return ""
	}
	commit := strings.TrimSpace(sctx.Config.TrustedConfigSHA)
	if commit == "" {
		commit = baseSHA
	}
	commands := map[string]string{
		"test":   sctx.Config.Commands.Test,
		"lint":   sctx.Config.Commands.Lint,
		"format": sctx.Config.Commands.Format,
	}
	pack, err := runbook.Build(sctx.Ctx, runbook.Inputs{
		RepoDir:            sctx.WorkDir,
		Commit:             commit,
		CacheDir:           sctx.RunbookDir,
		IncludeConventions: !sctx.Config.DisableProjectSettings,
		Commands:           commands,
		MaxBytes:           lean.RunbookMaxBytes,
	})
	if err != nil {
		sctx.Log(fmt.Sprintf("repository run-book unavailable, continuing without it: %v", err))
		return ""
	}
	if sctx.LogFile != nil {
		source := "built"
		if pack.Cached {
			source = "cached"
		}
		sctx.LogFile(fmt.Sprintf("repository run-book %s (%s, %d bytes)", pack.Key, source, len(pack.Text)))
	}
	return "\n" + pack.Text
}

// incrementalRereviewDelta decides whether this review turn may start with a
// delta-scoped pass, and returns the delta base and its changed paths. It
// applies only to a post-fix re-review whose round recorded the head it
// started from (the previous round's reviewed head), never to an initial
// review or a chunked one, and never when the delta is empty or unreadable -
// those cases fall back to the full-scope review.
func incrementalRereviewDelta(sctx *pipeline.StepContext, lean config.LeanTurns, chunked bool) (string, []string, bool) {
	if !lean.IncrementalRereview || !sctx.Fixing || chunked {
		return "", nil, false
	}
	deltaBase := strings.TrimSpace(sctx.ReviewStartingHeadSHA)
	if deltaBase == "" {
		return "", nil, false
	}
	raw, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", deltaBase)
	if err != nil {
		sctx.Log(fmt.Sprintf("could not read the fix-round delta since %s (%v); running the full-scope review", shortSHA(deltaBase), err))
		return "", nil, false
	}
	paths := changedPathList(string(raw))
	if len(reviewablePaths(paths, sctx.Config.IgnorePatterns)) == 0 {
		sctx.Log("the fix round left no reviewable delta; running the full-scope review")
		return "", nil, false
	}
	return deltaBase, paths, true
}

const maxDeltaPathsInPrompt = 40

// incrementalRereviewClause scopes a re-review to the fix-round delta. It
// narrows only where the reviewer looks first; the adversarial standard and
// every other rule above still apply, and the step cannot complete on this
// pass alone.
func incrementalRereviewClause(baseSHA, deltaBase string, paths []string) string {
	listed := paths
	more := ""
	if len(listed) > maxDeltaPathsInPrompt {
		more = fmt.Sprintf(", ... %d more", len(listed)-maxDeltaPathsInPrompt)
		listed = listed[:maxDeltaPathsInPrompt]
	}
	sanitized := make([]string, len(listed))
	for i, p := range listed {
		sanitized[i] = sanitizePromptText(p)
	}
	return fmt.Sprintf(`

Incremental re-review scope:
- The branch changes from %s through %s were fully reviewed in the previous round; its findings and the decisions on them are in the round history above.
- This pass reviews the fix-round delta: `+"`git diff %s`"+` (commits after %s plus any uncommitted worktree changes). Changed paths: %s%s.
- Verify that each previous finding the fix round addressed is actually resolved by the current code, and review the delta itself - including how it interacts with surrounding code, call sites, and tests - under the same standard as the original change.
- Do not re-review unchanged parts of the branch. Report an issue outside the delta only when the delta introduces it or makes it newly reachable.
- A full-scope review of the whole branch still runs before this review step can complete.
`, baseSHA, deltaBase, deltaBase, deltaBase, strings.Join(sanitized, ", "), more)
}
