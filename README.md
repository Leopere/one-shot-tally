# one-shot-tally

`one-shot-tally` records a Codex work cycle. It tracks tool use, checks, delivery, background jobs, work items, and subagent calls.

Use the report to find repeated calls, passive waits, redundant checks, unfinished work, and missing verification. The hook does not change commands or approve actions.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). All rights reserved. See [LICENSE](LICENSE).

## Outcomes

- `NO OBSERVED WORK`: no tool activity was recorded.
- `ACTIVITY OBSERVED`: activity occurred without verified current edits.
- `FAILED`: an unresolved delivery, edit, or other non-test action failed, or the requested outcome was abandoned.
- `VERIFIED`: native delivery succeeded for the current revision, or the current edit completed and a later standalone local check passed without changing the Git-visible worktree.
- `RECOVERED`: a delivery first failed, then the same operation later returned a matching structured success. The report marks this as `INCREDIBLE WIN` and gives the outcome grade 100. The efficiency score remains diagnostic.

The activity score is diagnostic. It does not change the recorded outcome. The score uses a workload allowance instead of one fixed tool-call limit.

Test failures are advisory. They remain in the check counts and report, but do
not automatically mark the requested outcome failed or override successful
delivery. Compare a failed expectation with the requested behavior: it may
reflect an intentional change, an unintended defect, a faulty test, or an
environment problem. Repair the relevant cause and continue. A failed test
does not become a pass, and passing tests alone do not prove deployment.

The base allowance is 30 weighted calls and three checks. Each accepted work item adds five calls and one check. Each completed item adds two calls. A distinct subagent task adds one call for coordination. The report uses at most 12 work items, seven added checks, and four subagent tasks. Repeated and failed additions reduce the score but do not expand the allowance. Long and repeated waits now reduce the efficiency score; they do not change the recorded outcome.

## Hook behavior

- `SessionStart` and `UserPromptSubmit` return `{}`.
- `PreToolUse` records calls and returns `{}`. It does not rewrite or approve a command.
- `PostToolUse` records explicit structured results and normally returns `{}`. Recognized browser startup errors and long or frequent waits add recovery guidance. Plain output text is not proof of success.
- Native `DeliveryResult` events record the actual `ship-it` or `deploy-it` result.
- Native command tracking includes literal Python paths and constant `Path` assignments outside the working repository. Only a changed repository enters the delivery queue; the hook does not scan neighbouring folders or run Python to discover paths.
- Structured command metadata and native delivery receipts supply result evidence. Prose, quoted examples, printed markers, and JSON printed in command stdout cannot prove success. Native receipts use a cooperating local executable protocol, not an authenticated remote service.
- Some Codex command hooks supply stdout without an exit status. Those results remain unknown. Native delivery reports its result directly after the repository command returns. A successful receipt must match a clean current checkout before it clears incomplete delivery.
- Unresolved results persist by session and project across turns. The first terminal blocker claim that is observed at `Stop` gets one bounded `decision:block` response that asks for fact checking and recovery.
- A repeated unresolved `Stop` does not continue the blocker response, but the changed outcome remains visible. An unresolved delivery is `FAILED` with outcome grade 0.
- Audit statements and quoted examples do not create blocker claims. The hook does not promise universal natural-language understanding.

Standard mode and goal mode use the same workload allowance.

## Current models

Tally observes the model selected by the runtime. It does not choose a model or infer token prices or quotas from role names. All models receive the same call weight. The legacy `spark_worker` role has no special discount.

Use Luna for routine work, bounded edits, and exploration. Use Sol for complex implementation and independent review. Reserve Astra for explicit escalation. Check available roles in the current session. Avoid copying old model IDs from saved reports.

The report includes `Observed models` when hook events supply a `model` field. It does not infer a child's actual model from a spawn request.

## Waits and check-ins

Observable `wait`, agent-wait, sleep, and empty-stdin polling calls count toward wait scoring. The first two brief continuations are free; passive sleeps and untracked idle polls retain their existing penalty. Each later wait costs five points. Each wait call's observed duration above 10 seconds adds five penalty points per started 10 seconds of aggregate excess. The total is capped at 70 points, with at least the existing passive-wait deduction, capped at 25 points. Bonuses cannot erase waiting costs.

Duration uses an observed elapsed time or a top-level native duration when available. A requested timeout is not elapsed time. Long waits or frequent polling can produce at most two advisory `PostToolUse` notices. Notices never veto a call or mark the outcome failed.

When an operation is still running, keep its existing operation handle and add one TODO check-in. Include the current status command, the next-check trigger, and the expected completion in `--context`. Continue independent work while it runs:

```sh
one-shot-tally todo add 'Check running build' \
  --context 'Operation: <existing job/handle>; status: <command>; check when <trigger>; expected completion: <result or time>'
```

If the operation does not finish, report the observed evidence and lack of progress, name the exact dependency or condition being awaited, and state the concrete action that will unblock it. Do not mark pending work complete. Do not restart an unknown external write until its result is known. For detached jobs, use `background record` and let the job call `background complete ID --wake` when it finishes.

A test rerun after a new observed edit is not redundant. One relevant check after an edit can receive the full activity score. Structured result evidence is still required.

Parallel hooks use the same bounded kernel file lock as the delivery registry. A slow active holder cannot lose its lock because its timestamp is old. Lock acquisition still times out after 1.5 seconds and fails open; contention can leave observations incomplete.

Version 1.23 migrates old state to neutral call weights and clears revision-blind repetition penalties, including in-flight test flags. Historical `spark_calls` remains readable for compatibility but is no longer incremented or displayed.

Code-mode batches still require structured child events to classify their checks. Tally does not execute or interpret arbitrary JavaScript to infer successful child work.

This tuning follows OpenAI's guidance to keep [skills and prompts focused on the requested outcome](https://developers.openai.com/blog/rethinking-skills-and-prompts-for-gpt-6-astra).

When a native Chrome or Chromium launch fails during startup, tally can advise the agent to change the browser runtime before retrying. Page errors and unrelated command failures do not trigger this guidance. See [browser startup failures](SKILL.md#browser-startup-failures) for the verified local alternative and its limits.

## Language rules

Use Microsoft Writing Style and ASD-STE100-inspired Simplified Technical English for documentation and command output.

- Lead with the result.
- Use short, direct sentences.
- Use one instruction per sentence or list item.
- Put a condition before its action.
- Use the same term for the same item.
- Keep exact command names, identifiers, and security terms.
- Keep hook output factual.
- Avoid legal and policy wording unless an exact field or command requires it.

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

## Background work

Ordinary shell removals use [Move to Trash with undo receipts](native-trash/README.md)
through the file guard. Each successful move prints the exact restore command.

The [fleet SSH guard](FLEET-SSH.md) is disabled. The full installer does not
activate it.

For the separate native macOS worker that removes Trash items older than seven days, see [automatic Trash maintenance](maintenance/README.md). It runs through launchd without AI input and requires one host-side activation.

Record a job before detaching it:

```sh
one-shot-tally background record docs-build --cleanup 'tmux kill-session -t docs-build'
```

Let the detached job report completion:

```sh
one-shot-tally background complete docs-build --wake
```

`record` captures `$TMUX_PANE` when available. `complete` is idempotent. Use `--wake` only from the detached job. Cleanup commands stay in state. The hook does not type cleanup commands into a terminal.

## Work items

For multi-step work, add one item for each distinct result. Do not add a work item for a trivial one-step request.

```sh
one-shot-tally todo add 'Review cache invalidation path' \
  --context 'Required before the cache release'
```

Use `todo list` to review entries. Use `todo done ID` when the result is complete. The report shows accepted items, add attempts, repeated adds, completed items, and open items.

Delegate an item only when it is independent and parallel work improves the result. Use a distinct `task_name` for each subagent task. The report shows total, distinct, and repeated subagent calls.

## Resume a goal

```sh
one-shot-tally goal list
one-shot-tally goal show ID
one-shot-tally goal resume ID
```

Add `--all` to include completed goals. `goal resume` prints the stored objective and the next two commands. The command does not change goal state.

## Encrypted credential delivery

Check the recipient key without reading or sending a credential:

```sh
one-shot-tally credential key-check
```

The command performs an isolated GnuPG `clear,wkd` lookup for `colin.knapp@boompay.ca`. It requires a valid self-certified UID and an encryption-capable key. It reports fingerprints for diagnostics but does not pin the recipient fingerprint in production.

Successful lookups are cached privately for one hour. Failed lookups are cached for five minutes. There is no embedded-key, local-keyring, DNS-record, keyserver, plaintext, or `gmail-cli` fallback.

To send, use a new operation ID and a non-secret account reference:

```sh
one-shot-tally credential send \
  --operation-id 123e4567-e89b-12d3-a456-426614174000 \
  --account boompay-admin
```

Pass the credential through stdin. Do not put it in an argument or environment variable.

The transport:

- signs with subkey `33EA65A9C078126556C150E1EA43219BE7B419F1`;
- encrypts to the validated WKD recipient key;
- sends only PGP/MIME ciphertext through a restricted SSH key;
- fixes the sender as `colin@nixc.us` and recipient as `colin.knapp@boompay.ca`;
- records metadata and ciphertext hashes, never credential text or a plaintext hash.

An operation ID is idempotent. Exit status 3 means the result is unknown. Resolve that receipt or mailbox state before creating another operation.

## Install

```sh
git clone https://github.com/Leopere/one-shot-tally.git
cd one-shot-tally
go test ./...
./install.sh
```

The installer builds `~/.local/bin/one-shot-tally`, copies `SKILL.md` to `~/.codex/skills/one-shot-tally/SKILL.md`, and verifies the installed version. Re-run it after each upgrade.

Use `./install.sh --tally-only` for isolated installation checks of version `1.23.0`. This mode builds and copies only `one-shot-tally` and its skill. It does not reinstall the file guard or its profile. The default `./install.sh` behavior remains the full install.

Installation does not enable hooks. The [Codex hook template](hooks/codex.json) configures `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop`, with five-second timeouts. The command quotes `$HOME` for paths containing spaces.

Merge these handlers into one active Codex hook source. Preserve other handlers. Account directories can share a hook file through symlinks; avoid registering the same handlers in multiple active sources. Use Codex's `/hooks` interface to review and trust new definitions. See the [official hooks documentation](https://learn.chatgpt.com/docs/hooks).

The pause file at `$HOME/.config/one-shot-tally/disabled` disables hook bookkeeping and installation. A paused hook immediately returns `{}`. CLI commands remain available, and `status --json` includes `hooks_paused`. An upgrade does not clear the pause.

Jenkins owns release builds, artifact publication, installation, and live acceptance. The current `.deploy-it.json` still points to a local installer and needs a Jenkins Mac CLI delivery path. Existing `build-one-shot-receiver`, `sign-one-shot-receiver`, and `deploy-one-shot-receiver` jobs target the separate Linux mail receiver. They do not establish that the Mac hook binary is deployed. Keep this gap visible when reporting delivery.

State defaults to `$HOME/.codex/state/one-shot-delivery`. Set `ONE_SHOT_STATE_DIR` for isolated testing. Goal history uses `$CODEX_HOME/goals_1.sqlite` for named accounts and otherwise `$HOME/.codex/goals_1.sqlite`.

## Development

```sh
go test ./...
go build ./...
```

Keep tests focused on workload accounting, hook silence, result evidence, revision ordering, concurrency, and credential transport boundaries.
