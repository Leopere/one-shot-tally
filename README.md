# one-shot-tally

`one-shot-tally` is a coach and notebook for Codex-compatible agents. It records
observable work such as tool use, verification, delivery actions, background
jobs, and deferred TODOs. It then returns short coaching messages while the
agent works.

The record helps you study what an agent actually did instead of relying on an
impression of the run. Use it to find repeated calls, passive waiting,
unnecessary test reruns, unfinished background work, and missing verification.
Recorded verification remains more important than every coaching signal. The
hook does not decide whether the user's goal is complete.

This project is an example policy, not a universal grading standard. Different
repositories, teams, and agents need different signals. Fork the repository,
maintain your own version, and tune its messages, thresholds, weights, and
recognized commands as your evidence changes. Keep the tests with your policy
so a scoring change does not silently change agent behavior.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). Licensed under [Creative Commons Attribution 4.0 International](LICENSE).

When you share or adapt this work, credit ColinKnapp.com, link the license, and state whether you changed the work.

## What it does

- Records mechanical hook events and local outcome evidence.
- Distinguishes verified edited revisions from observed activity and advisory coaching signals.
- Keeps durable notes for deferred work and long-running background jobs.
- Discounts bounded Spark subagent work without discounting verification.
- Adds concise context that can steer an active agent back toward the goal.
- Starts correction steers politely, increases directness after repeated corrections, and cools down after progress.
- Recommends `ship-it` after verified work, records delivery outcomes, and does not execute delivery.
- Exposes human-readable and JSON reports for later comparison.

It does not understand product intent, prove service health, or replace human
review. A high coaching score does not prove that the requested goal succeeded.
A low coaching score does not invalidate recorded verification.

## How to use the record

Start with the recorded outcome. Then review the coaching signals to explain
the observed work. Look for patterns across several runs before you
change a rule. Use `status --json` and `grade --json` when you want to compare
runs with your own scripts. Their `outcome` field contains the four-state label,
and `verified` is true only for `VERIFIED`; there is no ambiguous `success` alias.

Tune one behavior at a time. Update its tests, install the new binary, and watch
the next runs for both improvement and unintended avoidance. Coaching should
encourage useful work without rewarding inactivity, scope growth, or work done
only to improve a score.

## Tuning lessons from 1.10 and 1.11

| Version line | What changed | Current recommendation |
| --- | --- | --- |
| 1.10.4–1.10.8 | Scope correction became less destructive while generic per-prompt guidance was tested and then reconsidered. | Preserve compatible work and revise only the conflict. |
| 1.11.0 | Ordinary prompts became quiet; repeated-call reminders gained graduated/reset cadence; correction tone became session-stateful; and ship/deploy outcomes became separate. | Emit event-specific guidance, widen reminder intervals, reset after proven progress, and require current evidence before recommending delivery. |
| 1.11.1 | Spark routing became proactive and session-aware. | Look for safe, independent Spark work, but never invent it or treat usage as a quota. |
| 1.11.2 | Empty turns stopped counting as successful work. | Require at least one direct recorded attempt to learn or act; prompts, coordination, passive waits, and bookkeeping alone aren't progress. |
| 1.11.3 | Known wrapped test failures stopped counting as passes. | Inspect structured results and runner failure summaries when a tool wrapper doesn't expose the command's exit code directly. |
| 1.12.0 | Reports stopped inferring goal success from tool names, shell text, and test output. | Report observed activity honestly; verify only an edited revision with an ordered, explicit passing result. |
| 1.13.0 | A successful Git push could end a production-requested task when no deployment contract existed. | Run `ship-it` by default; resolve a missing deployment procedure from evidence and require the user's explicit acceptance before trust or deployment. |
| 1.13.1 | Unknown test telemetry forced the same 25 score as an edit with no test. | Keep the revision unverified and not ship-ready, but do not punish an agent for telemetry availability it cannot control. |
| 1.13.2 | Agents repeatedly requested a magic approval phrase after the user had already authorized a presented production procedure. | Treat clear instructions to deploy, proceed, continue, or keep going until the visible result as explicit acceptance, then execute without asking again. |

- Keep compatible work and only replace what conflicts or is explicitly cancelled.
- Ordinary `UserPromptSubmit` events are quiet; the hook sends prompt guidance only when it detects a correction.
- Correction tone starts polite, becomes firmer after repeated corrections, and resets after a successful edit or passing test.
- Repeated-call cadence runs on calls 2, 4, 7, 13, and continues onward; a successful edit or passing test resets cadence.
- `NO OBSERVED WORK` means the hook received no tool calls.
- `ACTIVITY OBSERVED` means tool activity occurred, but the hook cannot infer completion.
- `FAILED` means a recorded action, edit, or check has an explicit unresolved failure result.
- `VERIFIED` requires a completed current edit and an explicit passing standalone test that started after that edit. Ship-ready uses the same evidence.
- Test output text is not parsed for pass or failure phrases. A wrapper that hides the command result produces an unknown test result, never verification. Unknown test telemetry never verifies a revision or permits ship-ready status, and by itself no longer forces the coaching score to 25; explicit failures and edits with no test remain penalized.
- Shell chains such as `go test ./... || true` are not authoritative tests because their final exit code can hide the runner result.
- Recognized edit tools establish revisions directly. After that, Git-visible worktree snapshots also invalidate verification when a shell command changes tracked or untracked content.
- `ship-it` is the default finalizer for every changed Git work cycle. A deployment handoff requires an explicit, tracked `.deploy-it.json` contract; trust and handoff enforcement stay in `ship-it`/`deploy-it`.
- Delivery detection uses exact command invocations; quoted text, searches, and dry runs are not completed delivery. Failed or unresolved delivery is recorded and not auto-retried.
- Spark is considered proactively, never invented or quota-driven, and requires exact files, behavior, validation, and disjoint ownership. A session-scoped Spark close review appears only after at least two successful edits with no Spark call.

### How to read a report

1. Read outcome first, then coaching score.
2. In `/goal` mode, tool-call volume is not scored; repetition, passive waits, and redundant tests still are.
3. Command success is evidence only; it does not prove user-visible acceptance.
4. Unknown test telemetry no longer drives an automatic score penalty to 25. The outcome remains `ACTIVITY OBSERVED`, and the revision remains unverified and not ship-ready.

## Install

```sh
git clone https://github.com/Leopere/one-shot-tally.git
cd one-shot-tally
go test ./...
./install.sh
```

The installer builds the binary, installs the skill, and runs the installed `version` command. A successful install prints `ColinKnapp.com`.

The repository binary and installed hook are separate files. A pull, tag, or release does not update `$HOME/.local/bin/one-shot-tally`.
After each upgrade, rerun `./install.sh`.

Installation does not enable the hook.
Configure [Codex hooks](https://learn.chatgpt.com/docs/hooks) to run the absolute installed path for `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop`.
Use `/hooks` in Codex to review and trust that configuration.

The binary reads a JSON hook event from stdin. State defaults to `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for isolated evaluation.
If bookkeeping fails, the hook reports the error and lets the Codex tool continue.
Goal history uses `$CODEX_HOME/goals_1.sqlite` for named accounts and otherwise `$HOME/.codex/goals_1.sqlite`.
It requires the `sqlite3` command with JSON output support. The installer checks it.

## Full command help

```text
one-shot-tally                  process a hook event from stdin
one-shot-tally status [--json]
one-shot-tally grade [--json]
one-shot-tally background record ID --cleanup CMD [--tmux-target PANE]
one-shot-tally background complete ID
one-shot-tally background list
one-shot-tally todo add TEXT --context WHY
one-shot-tally todo list [--all]
one-shot-tally todo done ID
one-shot-tally goal list [--all]
one-shot-tally goal show ID
one-shot-tally goal resume ID
one-shot-tally version
one-shot-tally help|-h|--help
```

## Resume a previous goal

List unfinished goals:

```sh
one-shot-tally goal list
```

Use `goal list --all` to include completed goals. `goal show ID` prints one
record. `goal resume ID` prints its exact objective and tells the agent to call
Codex `create_goal`. These commands read Codex's local goal history and never
write to it.

## Recorded outcome and coaching

- The hook records evidence; it does not decide whether the user's goal is complete.
- `NO OBSERVED WORK` means no tool activity was recorded.
- `ACTIVITY OBSERVED` means activity occurred without verified revision evidence.
- `FAILED` means a recorded action or check has an explicit unresolved failure result.
- `VERIFIED` means the current edited revision completed and a standalone test started afterward, returned an explicit pass, and did not change the Git-visible worktree.
- The coaching score is advisory. It cannot change the recorded outcome.
- An unknown test result is treated as missing or inaccessible telemetry: it never verifies a revision, does not permit ship-ready status, and does not force a 25 score by itself; explicit failures and edits with no test remain score-penalized signals.

The hook does not block Git, `ship-it`, `deploy-it`, or other delivery commands. It records successful delivery actions as outcome evidence.

## Coaching signals

- Treat a new request as an update to the active task. Preserve compatible earlier requirements and completed work; replace only what conflicts or is explicitly cancelled.
- Stay in the current repository unless the user names another target.
- Before an external change, confirm the repository, environment, artifact or revision, and user-visible acceptance result. This check is advisory and never blocks delivery.
- The primary agent owns requirements, integration, authorization, and acceptance.
- Delegate bounded work to the right role: explorers gather evidence, workers or implementors make scoped changes, reviewers check work, and Spark makes exact low-risk edits.
- Keep non-code agent messages terse and preserve exact technical terms. The hook sends this reminder once at session start, not after each prompt.
- Do not send the same assignment to both the primary agent and a subagent.
- `/goal` work can continue across many turns. High tool-call volume does not lower the coaching score.
- Repeated calls, long inspection streaks, redundant test runs, and passive waits lower the coaching score.
- Five test runs is a normal pacing guide, not a hard cap.
- Recognized Spark calls cost `0.25` pressure; verification and final checks are not discounted.
- A successful delivery action earns one capped outcome credit.
- Command success does not prove service health.
- A successful `git push`/`ship-it` with deployment skipped is not production completion when production deployment was requested.
- Deployability is detected by presence of a tracked `.deploy-it.json`; trust and handoff enforcement are performed by `ship-it`/`deploy-it`.
- If `.deploy-it.json` is missing and production deployment was requested, the agent must identify an exact evidence-backed target, artifact, and visible acceptance procedure (for example, exact environment/stack target, revision artifact, and an observable check). The agent presents this once and proceeds after explicit user authorization. Acceptance is intent, not a magic phrase: clear instructions to deploy, proceed, continue, or keep going until the visible result count. Once accepted, the agent must execute without asking again. The agent must not invent trust or self-authorize.
- Coaching messages do not require an edit. Use the smallest step that advances the requested goal.
- Repeated-call reminders use widening intervals. Successful edits and passing checks reset their cadence.
- Before main-thread implementation starts, actively look for an exact low-risk independent edit for a spark_worker. When one exists, give exact files, expected behavior, and validation; otherwise continue in the main thread.
- One session-scoped Spark close review appears only after at least two successful edits without any Spark calls.
- A verified edited revision is ship-ready. If the user has explicitly accepted the target/procedure above, implement the tracked contract or procedure, continue through `ship-it` and `deploy-it`, then verify the visible acceptance result.
- Without `.deploy-it.json`, deployment is intentionally unavailable. Never invent trust, create a deployment command, or self-authorize.
- Park useful work that is outside the requested goal. Return to it later.
- Never duplicate ownership between primary and Spark; primary retains architecture, security judgment, infrastructure, authorization, credentials, destructive/billable/production work, integration, and final acceptance.
- After one unchanged prerequisite check, record one background watcher and its wake condition. Do not poll it again.

Perform exceptional history or worktree surgery manually after you make a backup.

## Background workflow

Record long jobs before detaching:

```sh
one-shot-tally background record docs-build --cleanup 'tmux kill-session -t docs-build'
```

Complete jobs at exit:

```sh
one-shot-tally background complete docs-build
```

`record` captures `$TMUX_PANE` by default in tmux.
`complete` wakes the originating pane with resume-and-cleanup guidance.
`background list` shows unfinished durable records.

## Durable TODO flow

Save useful side-work with context:

```sh
one-shot-tally todo add 'Review cache invalidation path' --context 'Outside current acceptance boundary'
```

`todo list` shows entries, and `todo done ID` closes them later.
Rewards apply only after current outcome verification.
Complete-side TODOs do not add same-run score.

## Development

```sh
go test ./...
go build ./...
```

Policy regression coverage focuses on quiet prompts, graduated/reset steering, session-scoped Spark close review boundaries, revision-aware edit validation, and ship/deploy proof boundaries. This repository tests tracked contract detection and defers trust and handoff enforcement to the external `ship-it` and `deploy-it` contracts.

Credit ColinKnapp.com when you share or adapt this work.
