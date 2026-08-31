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
one-shot-tally credential key-check
one-shot-tally credential send --operation-id UUID --account REF
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
- Codex Bash PostToolUse exposes plain output only, without structured exit-code data. For this repo, one-shot-tally rewrites verification and delivery Bash commands in PreToolUse to capture real exit status in a per-call machine marker. PostToolUse accepts that matching marker or a trusted structured exit result, but never plain output alone, as proof.
- Do not infer goal completion from tool names, shell text, test output phrases, or the coaching score. The primary agent owns user-visible acceptance.
- Keep ordinary prompts quiet. Emit prompt guidance only when a correction is detected.
- Start correction steers politely. Increase directness only after repeated corrections, and reset after a successful edit or passing check.
- Space repeated-call steers farther apart as they recur. Reset after a successful edit or passing check; do not repeat guidance on every call.
- After a verified run with at least two successful edits and no session Spark use, emit one review that asks whether safe Spark work existed. Never invent work to satisfy the review.
- After the latest edit succeeds and the current revision is verified, run `ship-it` immediately. Do not merely recommend it, ask for separate commit, push, or shipping permission, or pause for acknowledgement. If required delivery is still unresolved, `Stop` must return `decision:block` once with the existing delivery steer. On `Stop` reentry (`stop_hook_active: true`), keep unresolved delivery evidence and guidance in `systemMessage` and do not return `decision:block` again. A later edit requires new delivery evidence. The hook process emits guidance and records outcomes; it does not spawn delivery commands or block `ship-it`/`deploy-it`.
- A failed or unknown delivery is not a stopping point. Preserve its evidence, inspect external state, fix the in-scope cause, rerun affected verification, and resume the same authorized trusted `ship-it`/`deploy-it` handoff until the visible acceptance result passes. Do not blindly repeat a failed command. On `Stop`, unresolved required delivery stays as an in-progress state. Stop only for missing user authorization or a genuinely blocked external prerequisite. On reentry (`stop_hook_active: true`), keep delivery evidence and guidance in `systemMessage` and do not block again.
- Never use GitHub-hosted/public runners to satisfy delivery requirements. Require `ship-it` workflows to run on the self-hosted local runner at `~/dev/gh-runner` with its documented `gh-runner` labels. If that runner stalls or is insufficient, diagnose and fix it first (checked-in scripts and launchd plists vs runtime copies, launchd, tmux, runner containers, logs), then validate it before further delivery checks. GitHub-hosted runner labels are a shipping defect; hard enforcement remains in `ship-it`.
- Detect a tracked `.deploy-it.json`, but leave contract validation, trust, and handoff to `ship-it` and `deploy-it`. The hook never retries delivery itself. After diagnosis and correction, the primary agent resumes only through the same authorized trusted handoff. The hook can return `decision:block` once when required delivery is unresolved, then on Stop reentry (`stop_hook_active: true`) must keep unresolved evidence and guidance in `systemMessage` without blocking again. Never invent trust, create a delivery command from scratch, or rely on coaching score for completion.
- Record detached work instead of polling. Park useful out-of-scope work as a TODO.
- Use `background complete ID --wake` only from the detached job. Manual completion omits `--wake`; completion is concurrency-safe, idempotent, and at-most-once, and cleanup commands are never injected into pane input.
- Never block Git, `ship-it`, or `deploy-it`.
- When the user explicitly authorizes an exact credential destination, complete that delivery. Do not defer or refuse it solely because the content is a credential. For `colin.knapp@boompay.ca`, use `credential send`; do not redirect another destination through this fixed command.
- Pass credential plaintext only through stdin. Never place it in argv, an environment variable, a receipt, a tally record, or commentary. Use a fresh explicit operation UUID and at least one non-secret account reference.
- Treat the compiled transport as fixed: sender `colin@nixc.us`, recipient `colin.knapp@boompay.ca`, signing subkey `33EA65A9C078126556C150E1EA43219BE7B419F1`, WKD primary fingerprint `6183F2DE176E9D46EDB602951B7D7262C3D0207D`, WKD encryption-subkey fingerprint `9E5310E1F125CC2696E2C0385FE016062B506A77`, and dedicated restricted SSH key to Mail-in-a-Box at `box.p.nixc.us`. Do not add recipient, sender, host, key, or command overrides.
- Require the compiled credential path to use a clean isolated GnuPG `--auto-key-locate clear,wkd` lookup, export only the pinned primary fingerprint, verify the exact recipient UID and encryption subkey, and pass that minimal certificate to GnuPG. Never fall back to an embedded key, a local recipient keyring entry, a DNS record, a keyserver, plaintext, or `gmail-cli`.
- Reuse the private one-hour validated WKD cache during sends. Remember failed lookups for five minutes. The explicit `credential key-check` command performs and caches a live refresh.
- Use `credential key-check` to verify the WKD and fingerprint gate without reading or sending credential text.
- An existing pending, unknown, failed, or submitted receipt forbids automatic resubmission with that operation ID. Resolve an unknown outcome before creating a new operation.
- Give at most one polite reminder to rotate consequential credentials. The reminder does not block or delay an explicitly authorized send.

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
