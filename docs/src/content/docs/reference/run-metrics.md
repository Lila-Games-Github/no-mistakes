---
title: Run Metrics
description: Local phase timing artifacts, observation boundaries, and review-value measurements.
---

Each new run records content-free phase observations alongside its existing database evidence. Collection is advisory: it never changes validation, fixes, approval, publication, or CI behavior. Criticality tags describe the purpose of work and never waive a gate.

## Artifacts and export

The run log directory, `<NM_HOME>/logs/<run-id>/`, contains:

| File | Purpose |
| --- | --- |
| `phase-metrics.jsonl` | Versioned observations appended as operations finish, including during an active run |
| `metrics-health.json` | Writer seal with record count, dropped records, write failures, and interrupted segments |
| `run-metrics.v1.json` | Machine-readable snapshot generated at executor cleanup |
| `run-metrics.txt` | Summary of longest phases, repeated work, review findings, and criticality versus duration |

The files follow the existing run-log lifecycle. They survive worktree and test-evidence cleanup. An interrupted run may have only a partial journal; export reconstructs a snapshot from the database and available observations:

```sh
no-mistakes metrics <run-id>
no-mistakes metrics <run-id> --json > run-metrics.v1.json
```

Export queries the database read-only and does not drive the daemon or create a new run. A SQLite reader may create WAL coordination sidecars. Older runs can be exported with their original step, round, and invocation evidence; measurements that were not collected remain unknown.

## Version 1 contract

The top-level `schema_version` is `1`. Consumers should check it before interpreting fields and tolerate additive fields within a version. Durations are nonnegative milliseconds. `null` means unknown; it is distinct from a measured zero. Empty arrays mean no recorded entries.

| Field | Meaning |
| --- | --- |
| `run_id`, `status`, `generated_at` | Local correlation identity, current stored outcome, and UTC snapshot time |
| `timing` | Run wall time from database timestamps, sum of latest persisted step executions, and accumulated gate wait |
| `phases` | Each pipeline step's outcome, latest execution duration, criticality, cycle/fix counts, and finding counts; setup has its own journal-derived entry |
| `rounds` | Existing `step_rounds` duration and fix designation for each execution round |
| `invocations` | Existing agent duration, outcome/failure category, session mode, nullable per-round token/cache counters, workload size, tool counts, and findings |
| `details` | Journal spans with operation, owning step/round, timestamps, outcome/exit code, failure class, numeric input shape, and criticality |
| `observed_timing` | Cycle execution and observed active/external/human split across all recorded passes |
| `prior_branch_runs` | Earlier recorded runs on this branch; this is not a retry count |
| `collection_health`, `collection_complete` | Writer coverage; complete requires a sealed terminal snapshot with matching records and no drops, interruptions, or write failures |
| `journal_present`, `torn_journal` | Whether additional instrumentation exists and whether a partial trailing record was encountered |

`phases[].duration_recorded` distinguishes a pending step with no duration from an actual zero. The authoritative `duration_source` for pipeline steps is `step_results.duration_ms`; that field already excludes approval waits. A CI repair can reset it, so the report keeps `observed_cycle_ms`, `execution_cycles`, and `rerun_count` separately. `rerun_count` counts observed cycles after the first, including fix/rereview passes. It does not add them again to the latest persisted duration.

### Timing boundaries

Run wall time is creation through terminal update, or snapshot time while active. Database timestamps have second precision; monotonic operation durations have millisecond precision. The sum of phases need not equal wall time: setup, dispatch/cleanup overhead, crash downtime, and unobserved or reset execution can account for the difference.

Observed execution covers each `step.Execute` call, including its fix and rereview work. Active time is observed execution minus explicit external waits. It describes pipeline execution, including agent inference, tool subprocesses, and network operations; it is not CPU time. External wait currently covers CI polling sleeps and shared agent retry backoff. Provider-internal scheduling and transport waits cannot be separated reliably and remain inside execution. `invocations[].provider_queue_ms` remains `null`. Local agent dispatch is synchronous, with no queue; new agent spans record `input.dispatch_queue_ms: 0`.

Human wait means time parked at an approval gate, even when a supervising coding agent supplies the response. Run-level gate time comes from the existing durable `parked_ms` accounting and includes an active gate through the snapshot. Per-phase observed human waits cover the segments actually measured in this process; after recovery, run-level accounting remains authoritative for the whole gate.

Details include target/config/agent preparation, worktree preparation, fetch, rebase, diff, context construction, agent execution, configured validation commands, formatting, cycles, waits, and gates. Context construction spans local processing through the first agent dispatch in each cycle, including diff/history loading. `context_bytes` counts prompt plus JSON-schema bytes, not tokens or the agent's entire retrieved context. Workload lines count additions plus deletions from the existing diff-stat measurement. All detail spans may overlap parent phases and other details: **do not sum details into run wall time**.

The format command is measured inside Push. Configured tests and lint are command spans in their owning steps. Agent-authored build/format/test subprocesses stay inside agent execution; the existing adapter tool histogram and subprocess timing are exported where available. The current adapters do not provide distinct build or formatter durations, so those remain unknown rather than being inferred from command text. The combined document/lint pass stays attributed to its existing housekeeping invocation.

When collection is incomplete, observed aggregates describe available evidence, not a complete accounting; the derived active split remains `null`. Do not subtract two incomplete measurements to estimate missing work. The writer has a bounded queue; enqueue never waits for disk. Overflow increments the drop count, and storage failures never change a pipeline verdict. Cleanup drains the writer and replaces snapshots atomically. Recovery preserves complete journal records and removes only an unterminated tail before appending a new segment.

### Purpose and criticality

| Criticality | Current ownership |
| --- | --- |
| `required_correctness` | Target/worktree preparation, rebase, tests, CI |
| `risk_scaled_audit` | Review and its fix/rereview cycles |
| `supporting_validation` | Documentation, lint, formatting |
| `delivery_and_coordination` | Intent, push, PR work |

Detail observations inherit their step's tag, with formatting attributed to supporting validation. These tags are descriptive, not a scheduling or skip policy.

### Review value and privacy

Reported and resolved findings reuse the existing statistics owner. “Resolved” means a previously reported finding no longer appears in the final findings set; it does not prove that a fix was correct or that a review caught every defect. Compare these counts alongside fix rounds, repeated invocation/tool work, session reuse, cache reads, change size, and duration.

The artifact is an explicit allowlist of numeric measurements, categories, timestamps, and the local run ID. It excludes repository and branch names, paths, URLs, commands and arguments, config bodies, prompts, model/source contents, findings prose, raw errors, and resumable session identities. The local/remote collection boundary is owned by [the environment reference](/reference/environment/#what-stays-local-and-what-leaves-the-machine). The baseline workflow is in [Collecting a timing baseline](/guides/run-metrics-baseline/).
