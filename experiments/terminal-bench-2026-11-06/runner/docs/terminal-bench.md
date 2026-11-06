# Manual Terminal-Bench comparison

Run from the TTC checkout root with local Podman and host TTC credentials:

```sh
./prepare.sh 1         # prepare/resume task 1 and open TTC
./prepare.sh 2         # prepare/resume task 2 and open TTC
./prepare.sh shell 1   # optional observation shell in another terminal
./prepare.sh grade 1   # exit TTC first; snapshot and grade
```

Use `/model` inside TTC to select **GPT-6.1-Sol / max**, then type your own
prompt using `/task.md`. If that reasoning setting is unavailable, the run does
not exactly match the published configuration. Leave the easy model unset for a
single-model comparison. Do not use online task solutions or grader-specific
hints during solving.

| Task                     | Work                                                        | Published Codex successes |
| ------------------------ | ----------------------------------------------------------- | ------------------------- |
| 1: `mvcc-lsm-compaction` | Repair a C++ MVCC/LSM storage-engine failure                | 5/5                       |
| 2: `bun-sourcemap-leak`  | Repair a Bun/TypeScript release pipeline's provenance leaks | 0/5                       |

These are consistent outcomes in five published trials, not guarantees about
future runs. An
[independent audit](https://preferencemodel.com/blog/terminal-bench-4-is-saturated/issues/#bun-sourcemap-leak)
attributes Bun's failures to an ambiguous privacy requirement. The launcher
preserves the original instructions and tests; this task is a historical failure
baseline, not unambiguous evidence of a capability gap.

## Evidence and pins

The
[official submission](https://github.com/harbor-framework/terminal-bench/blob/main/leaderboard/submissions/2026-09-30-openai-gpt-6-1-sol-max-codex.json)
records Codex **0.159.0**, `openai/gpt-6.1-sol`, reasoning `max`, and source job
[`3837153f-6673-539d-b3e2-99a6edd3c5db`](https://hub.harborframework.com/jobs/3837153f-6673-539d-b3e2-99a6edd3c5db).
Its 330 trial IDs match the public Hub records: 66 tasks, five trials each, 192
successes. All ten selected trials completed without a recorded exception:

| Task | Trial IDs, each with the outcome above                                                                                                                                                                 |
| ---- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| MVCC | `08dd0651-460e-574d-b02b-4d99da01132e`, `5cfb3b19-fd3e-5348-873a-136a437ca1b8`, `75411780-c5b8-5995-bd20-6ca439218a8d`, `b321284e-e335-51a2-a670-9bc9e6a9cb9b`, `f33143b6-d02f-54ff-99ff-7cdc32940bdd` |
| Bun  | `10ae81f1-53f4-5600-984b-1c88a7422640`, `4e299280-f267-5f9f-9aec-fc06eae46e15`, `606a6fea-8eec-5961-8bb4-31c99bde5c9e`, `e8821293-6275-54ad-94ff-4dc64421b618`, `efb813f1-230b-5f2a-8268-85e99fdd4017` |

Source is pinned to
[v4.0.0 commit `452bf305c6daa62fc59061d22133a7cbc7c1572e`](https://github.com/harbor-framework/terminal-bench/tree/452bf305c6daa62fc59061d22133a7cbc7c1572e).
The selected published task package references are:

- MVCC:
  `sha256:370406dd79cc164e50b73568de37c34eba1b0e30782d7dc03f87c38c68123137`
- Bun: `sha256:e55ae542f338f50f4468d940c7d7c93a45d34fd418dc6cc5883c1fedc753603a`

The script uses the official environment and verifier image digests from those
packages. Their packaged files match the source commit except that the package
manifests add image pins and verifier resource settings.

## Isolation, results, and limits

- Existing Terminal-Bench 2.0 downloads/runs are untouched. New source is in
  `term-bench/source-v4`; private state and results are in `term-bench/v4/TASK`.
- Only a private copy of `openai-auth.json` enters the agent container. Source,
  reference solutions, hidden tests, host home, and the Podman socket do not.
  Keep `term-bench/` private: it contains credentials and TTC history.
- Agent images add terminal tools. Grading uses an unmodified, digest-pinned
  verifier image with no TTC credentials or host mounts. Only declared artifacts
  are uploaded. Host snapshots containing symlinks or special files are rejected
  before upload rather than followed into host-private paths.
- Both containers get 2 CPUs and 4 GiB RAM. Grader limits are 900 seconds for
  MVCC and 600 seconds for Bun. The official 8-hour agent limit and 10 GiB
  storage quota are not enforced. Local Podman and subscription auth differ from
  the published Modal/API setup; these are manual comparisons, not leaderboard
  runs.
- Grading freezes the workspace under `app/` and blocks further TTC launches for
  that attempt. Each grading attempt saves a `grade.XXXXXX/` directory
  containing `verifier.log` and collected verifier files. Reward `1` means pass;
  `0` means fail. A nonzero test exit with reward `0` is normal; inspect logs
  for setup errors. Missing rewards and timeouts are infrastructure errors, not
  scores.
- If grading fails before producing a valid result, rerun the same `grade`
  command: it reuses the frozen snapshot and keeps earlier logs. The verifier is
  stopped on exit; both containers are retained for inspection.

For a fresh model attempt, exit TTC and any running grader, then use:

```sh
./prepare.sh cleanup 2   # archive Bun and remove its containers; use 1 for MVCC
./prepare.sh 2           # rebuild TTC and launch a clean attempt
```

Cleanup requires an explicit task number and only removes containers owned by
this checkout. It archives the run under
`term-bench/v4/TASK.saved-TIMESTAMP.RANDOM`, preserving TTC history,
credentials, the workspace snapshot and grading results. If no workspace
snapshot exists, cleanup saves one before removal. Cached images, downloaded
source, other tasks and previous archives remain. Repeating cleanup without an
active run is harmless.

Offline launcher checks:
`python3 -m unittest discover -s tests -p test_prepare.py`.
