---
title: Large-diff review replay (2026-10-20)
description: Paired finding and timing evidence for experimental chunked Review.
---

## Decision

`review.chunk_threshold_lines` stays disabled by default (`0`). The measured chunked reviewer did not establish a finding-recall or wall-time improvement on the recorded large changes. It is available only as a trusted-repository opt-in for further experiments; [the configuration reference](/no-mistakes/reference/repo-config/#reviewchunk_threshold_lines) owns its behavior and bounds.

The October 19 timing benchmark motivated this trial: completed ProjectWhite changes measured 42,000–87,000 changed lines, and firstmate large Review phases had a 26.36-minute median. The risk to test was a missed changed file or call site, so this replay compared finding matches and pending findings alongside tokens and wall time.

## Reproduction

This comparison used an isolated, worktree-local reflink copy of the recorded eval corpus and Git object pools. It did not run a production pipeline, touch the shared daemon, or push the reviewed changes. Both variants used `claude,model=claude-sonnet-5-5`, the same two captured case IDs, and a three-repeat plan. The single-turn control passed `--review-chunk-threshold-lines 0`; the experimental variant passed `501`. That override is confined to the replay sandbox and appears in the candidate identity. Each variant used the same case cohort (`4c57c3545e407c60`). The replay's duration metric sums agent invocation wall time in the Review step and excludes case restoration; it also excludes the few seconds of local diff partitioning. Token figures are provider-reported, and cache reads are shown separately from input and output.

With an isolated copy of the corpus installed under a local `NM_HOME`, the paired commands were:

```sh
for threshold in 0 501; do
  no-mistakes eval run --cases all \
    --case 01M322TTP3M99MPAHATVC7424M-01M32EY3VZQV8CBFTFJ4QRZP1Y \
    --case 01M1NB8222V4AJN19T24ZKRWWJ-01M1NBR4M97ZKCXJC7GVKMQ1ZM \
    --candidate claude,model=claude-sonnet-5-5 --repeats 3 \
    --review-chunk-threshold-lines "$threshold"
done
```

| Recorded case | Diff size | Gold findings |
| --- | ---: | ---: |
| firstmate `01M322TTP3M99MPAHATVC7424M-01M32EY3VZQV8CBFTFJ4QRZP1Y` | 4,236 changed lines; 25 paths | 2 |
| ProjectWhite `01M1NB8222V4AJN19T24ZKRWWJ-01M1NBR4M97ZKCXJC7GVKMQ1ZM` | 86,852 changed lines; 1,677 paths | 1 |

An audit of the zero-context patches found 251,690 bytes and two chunks for firstmate, and 5,715,319 bytes and 29 chunks for ProjectWhite. Both firstmate gold paths were assigned to chunk 1; ProjectWhite's `traits.csv.import` path was assigned to chunk 9. The planner's regression test reconstructs the input patch byte for byte from its ordered chunks and rejects a mismatch between patch sections and changed paths. The missed gold paths were present in the supplied context, so their misses are model-review misses rather than planner omissions.

## Paired result

The table uses the median and nearest-rank 95th percentile of **completed** Review invocation-wall samples per case and variant. With only two or three samples, p95 is the observed maximum, not a stable tail estimate. Exact and fuzzy matches use the eval scorer's finding-level cascade. Pending findings are unmatched and unadjudicated; they are not counted as false positives. Token columns sum provider-reported use for completed replays only; `input/output` shows the provider's input and output counters, with cache reads separately.

| Case | Review mode | Completed / planned | Exact / fuzzy gold matches | Gold missed in completed runs | Pending | p50 / p95 wall | Input / output tokens | Cache-read tokens |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| firstmate, 4,236 lines | Single turn | 3 / 3 | 0 / 0 | 6 / 6 | 0 | 30.738s / 33.532s | 60 / 186 | 1,354,600 |
| firstmate, 4,236 lines | Chunked, 2 turns | 2 / 3 | 0 / 0 | 4 / 4 | 4 | 71.933s / 86.239s | 20 / 52 | 301,740 |
| ProjectWhite, 86,852 lines | Single turn | 3 / 3 | 0 / 0 | 3 / 3 | 0 | 16.839s / 21.437s | 44 / 234 | 927,505 |
| ProjectWhite, 86,852 lines | Chunked, 29 turns | 2 / 3 | 0 / 0 | 2 / 2 | 32 | 495.548s / 519.985s | 354 / 1,173 | 8,447,653 |

The third chunked ProjectWhite replay failed at chunk 23/29 after 387.915s of recorded agent time; the third chunked firstmate replay failed at chunk 1/2 after 5.455s. In both cases Claude exited with status 1 and an empty stderr message. These partial attempts have no complete finding set or reported token total, and neither contributes to the chunked percentile or match totals. The evaluator records them as failures; the cause of the Claude exits remains open. Control session: `1792474879272678286-af992c34458e31f3`; chunked session: `1792475048829853883-eeb7f3e20383dd9e`. A session that lost repeats like these can now retry only the lost slots, keeping the completed samples and the cohort, with `eval run --resume <session-id>` ([reference](/no-mistakes/reference/eval/#resume-failed-repeats)); that capability did not exist when this evidence was recorded.

## Finding-level misses

The frozen labels name three true issues:

| Gold ID | Issue represented by the label |
| --- | --- |
| `acceptance-extraction-drops-criterion-conditions` | `bin/fm-jev-accept-check.sh` drops continuation lines and some prose acceptance criteria. |
| `triage-budget-splitting-breaks-single-call-bound` | `bin/fm-jev.sh` can split one triage batch into multiple synchronous requests, violating the single-call budget. |
| `csv-import-no-regression-guard` | The ProjectWhite `traits.csv.import` keep-file change has no automated guard against deletion of the four required sidecars and a return to `csv_translation` on export. |

Both completed variants missed all three labeled issues on every repeat: the firstmate case missed `acceptance-extraction-drops-criterion-conditions` and `triage-budget-splitting-breaks-single-call-bound`; ProjectWhite missed `csv-import-no-regression-guard`. There were no exact or fuzzy matches. The failed third chunked repeats produced no complete finding set, so they cannot establish finding-level recall. The completed pairs show no **additional** labeled miss from chunking because the single-turn control also found none. They also provide no evidence that chunking preserves recall when the control does find an issue.

## Interpretation

The corpus has only one ProjectWhite case with gold at this scale and two gold findings in the selected firstmate case. Chunking was slower in both completed pairs and produced two incomplete attempts, so it does not support default-on rollout. Because the control missed every gold issue too, this replay cannot establish whether chunking changes finding-level recall. The deterministic planner and its tests prove patch-byte and path coverage in the supplied prompts; they cannot prove a model reasoned correctly about every path. Further rollout requires a broader labeled large-diff cohort with control hits, adjudication of pending findings, and investigation of the Claude exit-1 failures.

These replay times cover the captured Review turn only. The October 19 timing benchmark's Review-phase medians include fixing and re-review work, so they are context for choosing this experiment, not a direct control for the table above.
