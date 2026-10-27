package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/eval"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// newEvalCmd deliberately does not call trackCommand. Eval cases and candidate
// results are local code data, so this command surface has no remote telemetry
// event and does not use the daemon.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Inspect and locally replay review evaluation cases",
		Long:  "Inspect automatically collected review cases, capture runs on demand, ingest confirmed post-PR misses as false-negative gold, and compare agent candidates pinned to an explicit model and reasoning effort. Eval never starts or uses the shared daemon.",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalCaptureCmd())
	cmd.AddCommand(newEvalMissCmd())
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalSetsCmd())
	cmd.AddCommand(newEvalReportCmd())
	cmd.AddCommand(newEvalRelabelCmd())
	return cmd
}

func openEvalStore() (*paths.Paths, *eval.Store, error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve eval paths: %w", err)
	}
	store, err := eval.Open(p.EvalDir())
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
	store.SetRepoNames(evalRepoNames(p))
	return p, store, nil
}

// evalRepoNames resolves the repository fingerprints a case carries back to
// human names from the locally registered repositories. It is best effort and
// display-only: when the pipeline database cannot be read, the dashboards fall
// back to the fingerprint rather than failing an eval command that otherwise
// needs no database at all.
//
// The read goes through db.OpenReadOnly, never db.Open. db.Open creates the
// database and runs every migration, so routing a display lookup through it
// made `eval sets`, `eval report`, and `eval run` initialize pipeline state on
// a machine that has none, and migrate the schema of a database a running
// daemon owns. A missing database is the ordinary case here (os.IsNotExist),
// not an error worth reporting: the dashboards simply show fingerprints.
func evalRepoNames(p *paths.Paths) map[string]string {
	database, err := db.OpenReadOnly(p.DB())
	if err != nil {
		return nil
	}
	defer database.Close()
	repos, err := database.GetRepos()
	if err != nil {
		return nil
	}
	return eval.RepoDisplayNames(repos)
}

func newEvalCaptureCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capture <run>",
		Short: "Capture every review pass from a run into local cases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, database, err := openResources()
			if err != nil {
				return err
			}
			defer database.Close()
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			if cfg, cfgErr := config.LoadGlobal(p.ConfigFile()); cfgErr == nil {
				store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
			}
			cases, err := eval.Capture(cmd.Context(), store, p, database, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "captured %d local review case(s)\n", len(cases))
			for _, c := range cases {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  run %s round %s\n", c.ID, c.SourceRunID, c.SourceRoundID)
			}
			return nil
		},
	}
}

func newEvalMissCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "miss",
		Short: "Ingest confirmed post-PR review misses as false-negative gold",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newEvalMissIngestCmd())
	return cmd
}

func newEvalMissIngestCmd() *cobra.Command {
	var findings []string
	cmd := &cobra.Command{
		Use:   "ingest <run> --finding <json>",
		Short: "Capture a green review pass and label confirmed post-PR misses as false-negative gold",
		Long:  "Reads confirmed post-PR misses from --finding JSON (repeatable), not from GitHub comments or an external ledger. Captures the named run if needed, then writes false-negative gold onto the last completed non-blocking review pass.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(findings) == 0 {
				return fmt.Errorf("at least one --finding is required")
			}
			misses := make([]eval.FindingGold, 0, len(findings))
			for _, raw := range findings {
				miss, err := eval.ParsePostPRMissFinding(raw)
				if err != nil {
					return err
				}
				misses = append(misses, miss)
			}
			p, database, err := openResources()
			if err != nil {
				return err
			}
			defer database.Close()
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			result, err := eval.IngestPostPRMiss(cmd.Context(), store, p, database, args[0], misses)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ingested %d false-negative gold finding(s) into case %s (%d total)\n", result.Added, result.CaseID, result.Total)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&findings, "finding", nil, "confirmed miss as JSON finding object with id and description, optional file, line, severity (error|warning|info, default error) and action (auto-fix|ask-user|no-op) (repeatable)")
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	var cases string
	var candidateRaw string
	var repeats int
	var reviewChunkThreshold int
	var leanTurns string
	var reviewChunkMaxLines int
	var caseIDs []string
	var resumeSession string
	cmd := &cobra.Command{
		Use:   "run --cases <all|labeled|diversified|tune> --candidate <agent,model=...[,effort=...]> [--resume <session-id>]",
		Short: "Replay captured review passes and score findings against gold",
		Long: "Replays captured review passes through the Review step and scores the findings against gold.\n\n" +
			"With --resume <session-id>, only the (case, repeat) slots whose latest attempt is missing or failed are replayed. " +
			"That session's case set, repeat count, candidate identity, and review overrides are its own, so a retried slot joins the comparison it was planned for, " +
			"and each retry is recorded as a new attempt beside the failed row instead of overwriting it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if resumeSession != "" {
				// The recorded session is the authority for the comparison: a
				// flag that could change the case set, the candidate identity,
				// the repeat count, or the review context would retry slots into
				// a group the session never compared.
				for _, name := range []string{"cases", "candidate", "repeats", "case", "review-chunk-threshold-lines", "review-chunk-max-lines", "lean-turns"} {
					if cmd.Flags().Changed(name) {
						return fmt.Errorf("--%s cannot be combined with --resume: session %s already records the case set, candidate identity, repeat count and review overrides", name, resumeSession)
					}
				}
			} else {
				if cases == "" {
					return fmt.Errorf("--cases is required unless --resume <session-id> retries a recorded session")
				}
				if candidateRaw == "" {
					return fmt.Errorf("--candidate is required unless --resume <session-id> retries a recorded session")
				}
			}
			var candidate eval.Candidate
			if candidateRaw != "" {
				parsed, err := eval.ParseCandidate(candidateRaw)
				if err != nil {
					return err
				}
				candidate = parsed
			}
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			out := cmd.OutOrStdout()
			var plan eval.ReplayPlan
			var chunkOverride *int
			if cmd.Flags().Changed("review-chunk-threshold-lines") {
				chunkOverride = &reviewChunkThreshold
			}
			var chunkMaxLinesOverride *int
			if cmd.Flags().Changed("review-chunk-max-lines") {
				chunkMaxLinesOverride = &reviewChunkMaxLines
			}
			var leanOverride *bool
			if cmd.Flags().Changed("lean-turns") {
				switch leanTurns {
				case "on":
					leanOverride = new(bool)
					*leanOverride = true
				case "off":
					leanOverride = new(bool)
				default:
					return fmt.Errorf("--lean-turns must be on or off, got %q", leanTurns)
				}
			}
			session, evaluations, runErr := eval.Replay(cmd.Context(), store, eval.ReplayOptions{
				Set:                       cases,
				Candidate:                 candidate,
				Repeats:                   repeats,
				CaseIDs:                   caseIDs,
				ResumeSessionID:           resumeSession,
				ReviewChunkThresholdLines: chunkOverride,
				ReviewChunkMaxLines:       chunkMaxLinesOverride,
				LeanTurns:                 leanOverride,
				OnPlan: func(p eval.ReplayPlan) {
					plan = p
					switch {
					case p.Resumed && len(p.Slots) == 0:
						fmt.Fprintf(out, "session %s has no failed or missing slot out of %d planned; nothing to resume\n\n", p.Session.ID, p.Total)
					case p.Resumed:
						fmt.Fprintf(out, "resuming session %s: %d of %d planned slot(s) missing or failed, with %s on %s (cohort %s)\n\n",
							p.Session.ID, len(p.Slots), p.Total, p.Session.Candidate, p.Session.Set, p.Session.Cohort)
					default:
						fmt.Fprintf(out, "replaying %d case(s) x %d repeat(s) with %s on %s (cohort %s)\n\n",
							len(p.Session.CaseIDs), p.Session.Repeats, p.Session.Candidate, p.Session.Set, p.Session.Cohort)
					}
				},
				OnResult: func(evaluation eval.Evaluation, completed, total int) {
					evalRunProgress(out, evaluation, completed, total)
				},
			})
			if len(evaluations) > 0 {
				fmt.Fprintln(out)
				fmt.Fprintln(out, renderEvalRunSummary(plan.Session, evaluations, plan))
			}
			fmt.Fprintf(out, "local eval session %s: %d replay(s), candidate %s, repeats %d\n", session.ID, len(evaluations), session.Candidate, session.Repeats)
			if runErr != nil {
				return runErr
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cases, "cases", "", "case set: all, labeled (finding-level gold), diversified (official gold-only holdout), or tune")
	cmd.Flags().StringVar(&candidateRaw, "candidate", "", eval.CandidateUsage())
	cmd.Flags().IntVar(&repeats, "repeats", 3, "replays per case (minimum 1)")
	cmd.Flags().IntVar(&reviewChunkThreshold, "review-chunk-threshold-lines", 0, "isolated replay override: 0 keeps one review turn; a value over 500 enables chunking above that diff size")
	cmd.Flags().IntVar(&reviewChunkMaxLines, "review-chunk-max-lines", 0, "isolated replay override: changed lines per chunked review turn (0 keeps the byte budget only)")
	cmd.Flags().StringVar(&leanTurns, "lean-turns", "", "isolated replay override: on applies lean_turns to the candidate agent, off disables it")
	cmd.Flags().StringArrayVar(&caseIDs, "case", nil, "replay only this exact captured case ID (repeatable)")
	cmd.Flags().StringVar(&resumeSession, "resume", "", "retry only the failed or missing case-repeat slots of this recorded eval session id")
	return cmd
}

func newEvalSetsCmd() *cobra.Command {
	var refresh bool
	cmd := &cobra.Command{
		Use:   "sets",
		Short: "Inspect local case-set size, finding-level gold, and composition",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			if refresh {
				if _, err := store.RefreshDiversified(); err != nil {
					return err
				}
			}
			summaries, err := eval.InspectSets(store)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), renderEvalSetsDashboard(summaries))
			return nil
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh-diversified", false, "rebuild the official diversified pin set from current gold")
	return cmd
}

func newEvalReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Report local true-positive / false-negative scores, tokens, and cost",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := openEvalStore()
			if err != nil {
				return err
			}
			defer store.Close()
			reports, err := eval.Report(store)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), eval.RenderReport(reports))
			return nil
		},
	}
}

func newEvalRelabelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "relabel [run]",
		Short: "Refresh gold labels when a source PR has merged after capture",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, database, err := openResources()
			if err != nil {
				return err
			}
			defer database.Close()
			store, err := eval.Open(p.EvalDir())
			if err != nil {
				return err
			}
			defer store.Close()
			if cfg, cfgErr := config.LoadGlobal(p.ConfigFile()); cfgErr == nil {
				store.SetDiversifiedSize(cfg.Eval.DiversifiedSize)
			}
			var cases []eval.Case
			if len(args) == 1 {
				cases, err = eval.RelabelRun(cmd.Context(), store, p, database, args[0])
			} else {
				cases, err = eval.RelabelAll(cmd.Context(), store, p, database)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "relabeled %d local review case(s)\n", len(cases))
			return nil
		},
	}
}
