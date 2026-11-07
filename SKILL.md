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

The hook records activity, checks, delivery, background work, work items, and subagent calls.

- `NO OBSERVED WORK`: no tool activity.
- `ACTIVITY OBSERVED`: activity without verified current edits.
- `FAILED`: an unresolved delivery, edit, or other non-test action failed, or the requested outcome was abandoned.
- `VERIFIED`: a native Git shipping receipt succeeded for the current revision, or the current edit completed and a later standalone local check passed.
- `RECOVERED`: a failed delivery later has a matching structured success. The report shows `INCREDIBLE WIN` and outcome grade 100. Efficiency remains diagnostic.

Test failures are advisory. Keep them visible without automatically marking the
requested outcome failed or overriding successful delivery. Compare each failed
expectation with the requested behavior. Diagnose intentional changes,
unintended defects, faulty tests, or environment problems; repair the relevant
cause and continue. Failed tests do not become passing tests. Verify the actual
deployment independently.

`SessionStart`, `UserPromptSubmit`, and `PreToolUse` return `{}`. `PostToolUse` normally returns `{}`. Recognized browser startup errors and long or frequent waits add recovery guidance. The hook does not change or approve commands.

The first `Stop` reports the recorded facts. A repeated `Stop` returns `{}` unless the blocker or recovery report changed.

Native `DeliveryResult` events record Git shipping results. Structured command metadata can also supply result evidence. Legacy deployment commands do not prove deployment success; use the repository Jenkins command for deployment acceptance.

Some Codex command hooks supply stdout without an exit status. Those results remain unknown. A successful shipping receipt must match a clean current checkout before it clears incomplete delivery.

Unresolved outcomes persist by session and project across turns. The first terminal blocker claim observed at `Stop` gets one bounded `decision:block` response that asks for fact checking and recovery. A repeated unresolved `Stop` does not continue the response, but the changed outcome remains visible. An unresolved delivery is `FAILED` with outcome grade 0.

Audit statements and quoted examples do not create blocker claims. The hook does not promise universal natural-language understanding.

The workload score is diagnostic. Track distinct results when durable work items help. Complete an item when its result is verified. Delegate independent work when it improves the result, and give each task a distinct `task_name`.

Do not add work items, subagents, or tests just to improve the score. One relevant check can be sufficient. After a repair, rerun the affected check.

## Models and hooks

Use configured roles and models available in the current session. Astra, Sol, and Luna receive the same activity accounting. The legacy `spark_worker` name has no discount or separate quota.

The hook records the runtime's `model` field when supplied. Missing metadata stays unknown. A requested child model does not prove that child's actual model use or cost.

Observable `wait`, agent-wait, sleep, and empty-stdin polling calls count toward wait scoring. The first two brief continuations are free; passive sleeps and untracked idle polls retain their existing penalty. Each later wait costs five points. Each wait call's observed duration above 10 seconds adds five penalty points per started 10 seconds of aggregate excess. The total is capped at 70 points, with at least the existing passive-wait deduction, capped at 25 points. Bonuses cannot erase waiting costs.

Use observed elapsed time or a top-level native duration when available. A requested timeout is not elapsed time. Long waits or frequent polling can produce at most two advisory `PostToolUse` notices. Notices never veto calls or mark the outcome failed.

When an operation is still running, keep its existing operation handle and add one TODO check-in. Put the current status command, next-check trigger, and expected completion in `--context`. Continue independent work while it runs:

```sh
one-shot-tally todo add 'Check running build' \
  --context 'Operation: <existing job/handle>; status: <command>; check when <trigger>; expected completion: <result or time>'
```

If the operation does not finish, report the observed evidence and lack of progress. Name the exact dependency or condition being awaited, then state the concrete action that will unblock it. Do not mark pending work complete. Do not restart an unknown external write until its result is known. For detached jobs, use `background record` and let the job call `background complete ID --wake` when it finishes.

Tests repeated after a new observed edit are not redundant checks. Batched JavaScript calls remain opaque without structured child evidence; printed output cannot establish verification.

The [Codex hook template](hooks/codex.json) includes five lifecycle events with five-second timeouts. Merge handlers into one active config source; do not overwrite other hooks. Codex requires review and trust for new hook definitions.

When `$HOME/.config/one-shot-tally/disabled` exists as a file, the hook returns `{}` without reading input or writing state. CLI commands still work. `status --json` reports `hooks_paused`. Preserve an existing pause unless the user requests activation.

## Browser startup failures

If Chrome aborts during macOS application registration, do not repeat the same native launch unchanged. Check the failure output and the available browser runtimes first. Use an alternate browser runtime or service for the same test.

On the local Mac, a native Chrome startup crash was matched to a Playwright launch on September 7, 2026. The installed `ghcr.io/browserless/chromium:v2.55.2` image subsequently passed a Playwright launch and button-click check with networking disabled and no host mounts. In that image, `playwright-core` selects its bundled Chromium with `chromium.launch({headless:true})`. Verify the image remains available before using it. Supply only the test files and network access that the task requires.

This guidance does not prove why macOS registration failed. If no permitted runtime works, report the exact failure and continue independent work.

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

The full installer also installs native Move to Trash routing for ordinary shell
removals. Include each successful move's printed Undo command in the user reply.
Do not claim a move succeeded before the tool returns.

```sh
go test ./...
./install.sh
```

The installed version line includes `ColinKnapp.com`.

Use `./install.sh --tally-only` for isolated installation checks of version `1.23.0`. It does not reinstall the file guard or profile. The default installer still performs the full install. Both modes respect the pause file.

Jenkins job `deploy-one-shot-tally` owns release builds, installation, and live verification at `/Users/aedev/.local/bin/one-shot-tally`. After pushing the exact revision to `main`, run `python3 scripts/jenkins-delivery.py`; it verifies local `HEAD` matches `origin/main` and submits that revision to Jenkins. The accepted artifact records the revision, binary digest, live checks and delivery timings. The Linux mail receiver uses separate jobs.

State defaults to `$HOME/.codex/state/one-shot-delivery`. Set `ONE_SHOT_STATE_DIR` to use another path.
