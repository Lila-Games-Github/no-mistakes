---
title: Collecting a Timing Baseline
description: Compare representative runs before proposing changes to review speed.
---

Collect comparable run artifacts before changing behavior. Use [the run-metrics reference](/reference/run-metrics/) for field definitions and observation limits. Criticality-versus-duration is evidence for discussion; it cannot authorize skipping a gate.

## First comparison point

The Lila fork installation proof from September 28, 2026, recorded run `01M3K2DF487NMDMRYQ09438557` on build `v1.58.1-lila-target-branch-c02ebab`. Its existing database step durations were:

| Phase | Seconds |
| --- | ---: |
| Review | 22.605 |
| Test | 60.636 |
| Document | 17.284 |
| CI | 70.308 |

All executed steps totaled 191.838 seconds; creation through terminal update took approximately 194 seconds. There were no fix rounds or parked waits. Lint was explicitly skipped because the managed daemon lacked Go in its PATH, after local lint passed. The throwaway target branch had no CI checks; CI time included polling, observation, and intentional closure. This is a routing proof, not a representative Frogpile baseline or a measurement of remote CI execution. Input/context size and active/external splits were not recorded and must remain unknown.

This proof data comes from Firstmate's fork-install report. The matching legacy-duration regression fixture in `internal/runmetrics/report_test.go` checks that exporting older evidence preserves the durations without fabricating new measurements.

## Representative Frogpile collection

After the telemetry build is merged and installed by the operator, export each representative run:

```sh
no-mistakes metrics <run-id> --json > <run-id>.metrics.json
no-mistakes metrics <run-id> > <run-id>.metrics.txt
```

Keep the same build, target branch, agent configuration, and gate configuration for comparable samples. Record that configuration separately without credentials. Include a small clean change, an ordinary change, a broad change, and a run with real review fixes or CI revalidation when those occur naturally. Record the actual changed-file/line and context/token measurements; do not label the proof run “representative” to fill a missing workload category.

Compare longest phases, repeat counts and observed cycle time, review findings remaining after fixes, cache/session reuse, and tool/command activity. Treat human gates, CI polling, retry backoff, unknown provider queue time, and incomplete collection separately. Preserve the original artifacts and use multiple samples rather than drawing a speed conclusion from one run.

Representative Frogpile artifacts have not been collected as part of the instrumentation change. No review-speed or gate behavior changes are included. The next review-speed investigation should first collect this baseline, then name the repeated work or slow boundary its proposal addresses and retain the correctness contract in its validation plan.
