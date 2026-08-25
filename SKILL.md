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
one-shot-tally background complete ID [--wake]
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
- Before external changes, identify and state the target, artifact or revision, and visible acceptance result. This confirmation is an evidence check, not a permission request. Continue without waiting for acknowledgement when the target is already clear.
- Treat a standing user instruction to ship completed changes to production as explicit authorization for later matching revisions through an already-trusted tracked deployment contract until the user revokes it. Do not ask for per-revision permission.
- Only the user may authorize a new production target or deployment trust. When no matching authorization exists, present the exact target and visible acceptance procedure once. Treat acceptance as intent, not a magic phrase, and do not ask again once accepted.
- Keep requirements, integration, authorization, and acceptance with the primary agent.
- Before main-thread implementation, actively look for an exact low-risk, independent edit for Spark. When one exists, give exact files, expected behavior, and validation.
- Keep architecture, security judgment, infrastructure, authorization, credentials, destructive/billable/production work, and final acceptance in the primary thread.
- Do not duplicate ownership between primary and Spark; assign disjoint edits only.
- Delegate bounded work to the right role: explorers gather evidence, workers or implementors make scoped changes, reviewers check work, and Spark makes exact low-risk edits.
- Keep non-code agent messages terse and preserve exact technical terms.
- Never assign the same task to both the primary agent and a subagent.
- Treat coaching as advisory; never trade correctness or delivery for a score.
- Treat hook evidence honestly: no calls are `NO OBSERVED WORK`; calls without verified revision evidence are `ACTIVITY OBSERVED`; explicit failures are `FAILED`; only a completed current edit followed by an explicit passing standalone check is `VERIFIED`.
- When wrapping `exec_command`, return its complete machine-readable result, including `exit_code`; do not return only stdout. Never infer success from human-readable output.
- Do not infer goal completion from tool names, shell text, test output phrases, or the coaching score. The primary agent owns user-visible acceptance.
- Keep ordinary prompts quiet. Emit prompt guidance only when a correction is detected.
- Start correction steers politely. Increase directness only after repeated corrections, and reset after a successful edit or passing check.
- Space repeated-call steers farther apart as they recur. Reset after a successful edit or passing check; do not repeat guidance on every call.
- After a verified run with at least two successful edits and no session Spark use, emit one review that asks whether safe Spark work existed. Never invent work to satisfy the review.
- After the latest edit succeeds and the current revision is verified, run `ship-it` immediately. Do not merely recommend it, ask for separate commit, push, or shipping permission, or pause for acknowledgement. The hook process emits guidance and records outcomes; it does not spawn delivery commands itself.
- Never use GitHub-hosted/public runners to satisfy delivery requirements. Require `ship-it` workflows to run on the self-hosted local runner at `~/dev/gh-runner` with its documented `gh-runner` labels. If that runner stalls or is insufficient, diagnose and fix it first (checked-in scripts and launchd plists vs runtime copies, launchd, tmux, runner containers, logs), then validate it before further delivery checks. GitHub-hosted runner labels are a shipping defect; hard enforcement remains in `ship-it`.
- Detect a tracked `.deploy-it.json`, but leave contract validation, trust, and handoff to `ship-it` and `deploy-it`. Never retry failed delivery automatically.
- Record detached work instead of polling. Park useful out-of-scope work as a TODO.
- Use `background complete ID --wake` only from the detached job. Manual completion omits `--wake`; completion is concurrency-safe, idempotent, and at-most-once, and cleanup commands are never injected into pane input.
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
