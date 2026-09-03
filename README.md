# one-shot-tally

`one-shot-tally` records a Codex work cycle. It tracks tool use, checks, delivery, background jobs, work items, and subagent calls.

Use the report to find repeated calls, passive waits, redundant checks, unfinished work, and missing verification. The hook does not change commands or approve actions.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). All rights reserved. See [LICENSE](LICENSE).

## Outcomes

- `NO OBSERVED WORK`: no tool activity was recorded.
- `ACTIVITY OBSERVED`: activity occurred without verified current edits.
- `FAILED`: a recorded action or check has an explicit unresolved failure.
- `VERIFIED`: the current edit completed and a later standalone local check passed without changing the Git-visible worktree.

The activity score is diagnostic. It does not change the recorded outcome. The score uses a workload allowance instead of one fixed tool-call limit.

The base allowance is 30 weighted calls and three checks. Each accepted work item adds five calls and one check. Each completed item adds two calls. A distinct subagent task adds one call for coordination. The report uses at most 12 work items, seven added checks, and four subagent tasks. Repeated and failed additions reduce the score but do not expand the allowance.

## Hook behavior

- `SessionStart` and `UserPromptSubmit` return `{}`.
- `PreToolUse` records calls and returns `{}`. It does not rewrite or approve a command.
- `PostToolUse` records explicit structured results and returns `{}`. Plain output text is not proof of success.
- The first `Stop` reports the detailed tally. A repeated `Stop` returns `{}`.

Standard mode and goal mode use the same workload allowance.

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

Delegate an item only when it is independent and parallel work improves the result. Use a distinct `task_name` for each subagent task. The report shows total, distinct, Spark, and repeated subagent calls.

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

Installation does not enable hooks. Configure Codex to run the absolute installed path for the hook events you want. The supplied setup supports `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop`.

State defaults to `$HOME/.codex/state/one-shot-delivery`. Set `ONE_SHOT_STATE_DIR` for isolated testing. Goal history uses `$CODEX_HOME/goals_1.sqlite` for named accounts and otherwise `$HOME/.codex/goals_1.sqlite`.

## Development

```sh
go test ./...
go build ./...
```

Keep tests focused on workload accounting, hook silence, result evidence, revision ordering, concurrency, and credential transport boundaries.
