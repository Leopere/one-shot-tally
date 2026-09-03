---
name: one-shot-tally
description: Use one-shot-tally for work status, workload-aware scoring, goal recovery, background jobs, and durable work items.
---

# One-Shot Tally

## Commands

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
one-shot-tally credential key-check
one-shot-tally credential send --operation-id UUID --account REF
one-shot-tally version
one-shot-tally help|-h|--help
```

## Behavior

The hook records activity, checks, delivery, background work, work items, and subagent calls. It never blocks an action.

- `NO OBSERVED WORK`: no tool activity.
- `ACTIVITY OBSERVED`: activity without verified current edits.
- `FAILED`: an explicit unresolved failure.
- `VERIFIED`: the current edit completed and a later standalone local check passed.

`SessionStart`, `UserPromptSubmit`, `PreToolUse`, and `PostToolUse` return `{}`. The hook does not change or approve commands.

The first `Stop` reports the recorded facts. A repeated `Stop` returns `{}`.

The score uses a workload allowance. Each accepted work item expands the allowance once. Completed items include closure overhead. Distinct subagent tasks include coordination overhead. Repeated or failed additions do not expand the allowance.

For multi-step work:

1. Add one TODO for each distinct result.
2. Complete each TODO when its result is verified.
3. Delegate only independent items that benefit from parallel work.
4. Give each subagent task a distinct `task_name`.

Do not create TODOs or subagents only to increase the score.

## Language

Use `$microsoft-writing-style` and `$sop-ste100-rewriter` for prose changes in this repository.

- Lead with the result.
- Use short, active sentences.
- Use one instruction per sentence or list item.
- Put a condition before its action.
- Keep exact command names, identifiers, and security terms.
- Keep hook output factual.
- Avoid legal and policy wording unless an exact field or command requires it.

Use `background complete ID --wake` only from the detached job. Manual completion omits `--wake`.

## Credential delivery

Use `credential send` only when the user asks to send a credential to `colin.knapp@boompay.ca`.

- Send plaintext through stdin only. Keep it out of arguments, environment variables, receipts, tally records, and commentary.
- Use a new operation UUID and a non-secret account reference.
- The transport fixes sender `colin@nixc.us`, recipient `colin.knapp@boompay.ca`, signing subkey `33EA65A9C078126556C150E1EA43219BE7B419F1`, host `box.p.nixc.us`, and a restricted SSH key.
- Resolve the recipient through isolated `clear,wkd`. Verify the exact UID and an encryption-capable key. Do not fall back to another key source or plaintext.
- Do not resubmit a pending, unknown, failed, or submitted operation ID. Resolve an unknown result first.

## Resume a goal

1. Call `get_goal`.
2. If a goal is active, continue that goal.
3. If no goal is active, list the saved goals.
4. Select the required goal ID.
5. Run `one-shot-tally goal resume ID`.
6. Call `create_goal` with the printed objective.

The command reads Codex goal history. It does not change Codex goal state.

## Build and install

```sh
go test ./...
./install.sh
```

The installed version line includes `ColinKnapp.com`.

State defaults to `$HOME/.codex/state/one-shot-delivery`. Set `ONE_SHOT_STATE_DIR` to use another path.
