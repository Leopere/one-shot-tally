---
name: one-shot-tally
description: Use one-shot-tally for concise coaching, goal recovery, background jobs, and durable TODOs.
---
# One-Shot Tally

## Command help

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

## Rules

- Finish the requested outcome and verify edits.
- Treat a new request as an update to the active task. Preserve compatible earlier requirements and completed work; replace only what conflicts or is explicitly cancelled.
- Before external changes, confirm the target, artifact or revision, and visible acceptance result.
- Keep requirements, integration, authorization, and acceptance with the primary agent.
- Before main-thread implementation, actively look for an exact low-risk, independent edit for Spark. When one exists, give exact files, expected behavior, and validation.
- Keep architecture, security judgment, infrastructure, authorization, credentials, destructive/billable/production work, and final acceptance in the primary thread.
- Do not duplicate ownership between primary and Spark; assign disjoint edits only.
- Delegate bounded work to the right role: explorers gather evidence, workers or implementors make scoped changes, reviewers check work, and Spark makes exact low-risk edits.
- Keep non-code agent messages terse and preserve exact technical terms.
- Never assign the same task to both the primary agent and a subagent.
- Treat coaching as advisory; never trade correctness or delivery for a score.
- Start correction steers politely. Increase directness only after repeated corrections, and reset after concrete progress.
- Space repeated-call steers farther apart as they recur. Do not repeat guidance on every call.
- At closing, move verified edits to `ship-it`. Let `ship-it` hand off to `deploy-it` only through an already trusted, tracked `.deploy-it.json` contract.
- Record detached work instead of polling. Park useful out-of-scope work as a TODO.
- Never block Git, `ship-it`, or `deploy-it`.

## Resume a goal

When the user asks to resume earlier work:

1. Run `one-shot-tally goal list` or `goal list --all`.
2. Run `one-shot-tally goal resume ID` for the selected goal.
3. Call `get_goal`. Do not replace an unfinished current goal.
4. Call `create_goal` with the exact objective printed by the command.

The command reads Codex goal history. It does not alter Codex goal state.

## Build and install

```sh
go test ./...
./install.sh
```
The installed version line includes `ColinKnapp.com`.

Default state: `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for alternate paths.
