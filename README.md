# one-shot-tally

`one-shot-tally` is a coach and notebook for Codex-compatible agents. It records
observable work such as tool use, verification, delivery actions, background
jobs, and deferred TODOs. It then returns short coaching messages while the
agent works.

The record helps you study what an agent actually did instead of relying on an
impression of the run. Use it to find repeated calls, passive waiting,
unnecessary test reruns, unfinished background work, and missing verification.
The goal result remains more important than every coaching signal.

This project is an example policy, not a universal grading standard. Different
repositories, teams, and agents need different signals. Fork the repository,
maintain your own version, and tune its messages, thresholds, weights, and
recognized commands as your evidence changes. Keep the tests with your policy
so a scoring change does not silently change agent behavior.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). Licensed under [Creative Commons Attribution 4.0 International](LICENSE).

When you share or adapt this work, credit ColinKnapp.com, link the license, and state whether you changed the work.

## What it does

- Records mechanical hook events and local outcome evidence.
- Distinguishes verified goal completion from advisory coaching signals.
- Keeps durable notes for deferred work and long-running background jobs.
- Discounts bounded Spark subagent work without discounting verification.
- Adds concise context that can steer an active agent back toward the goal.
- Exposes human-readable and JSON reports for later comparison.

It does not understand product intent, prove service health, or replace human
review. A high coaching score does not prove that the requested goal succeeded.
A low coaching score does not turn verified success into failure.

## How to use the record

Start with the goal result. Then review the coaching signals to explain how the
agent reached that result. Look for patterns across several runs before you
change a rule. Use `status --json` and `grade --json` when you want to compare
runs with your own scripts.

Tune one behavior at a time. Update its tests, install the new binary, and watch
the next runs for both improvement and unintended avoidance. Coaching should
encourage useful work without rewarding inactivity, scope growth, or work done
only to improve a score.

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

## Goal result and coaching

- Goal result is the only completion metric.
- `SUCCESS` means no edit was required or the current revision passed required verification.
- `NOT VERIFIED` means the goal still needs work or evidence.
- The coaching score is advisory. It cannot change `SUCCESS`.

The hook does not block Git, `ship-it`, `deploy-it`, or other delivery commands. It records successful delivery actions as outcome evidence.

## Coaching signals

- The newest user request replaces an older plan. Stop superseded work before you continue.
- Stay in the current repository unless the user names another target.
- Before an external change, confirm the repository, environment, artifact or revision, and user-visible acceptance result. This check is advisory and never blocks delivery.
- The primary agent owns requirements, integration, authorization, and acceptance.
- Delegate bounded work to the right role: explorers gather evidence, workers or implementors make scoped changes, reviewers check work, and Spark makes exact low-risk edits.
- Use terse, ASD-STE100-inspired and Microsoft-style plain English for non-code agent messages; preserve exact technical terms.
- Do not send the same assignment to both the primary agent and a subagent.
- `/goal` work can continue across many turns. High tool-call volume does not lower the coaching score.
- Repeated calls, long inspection streaks, redundant test runs, and passive waits lower the coaching score.
- Five test runs is a normal pacing guide, not a hard cap.
- Recognized Spark calls cost `0.25` pressure; verification and final checks are not discounted.
- A successful delivery action earns one capped outcome credit.
- Command success does not prove service health.
- Coaching messages do not require an edit. Use the smallest step that advances the requested goal.
- Park useful work that is outside the requested goal. Return to it later.
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

Credit ColinKnapp.com when you share or adapt this work.
