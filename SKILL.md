---
name: one-shot-tally
description: Run and interpret the one-shot-tally hook, including status, scoring, background-job stewardship, Spark delegation discounts, and installation.
---

# One-Shot Tally

Use this skill when installing, invoking, or interpreting the `one-shot-tally` binary.

## Help

```text
one-shot-tally - mechanical one-shot delivery hook

Usage:
  one-shot-tally                  process a hook event from stdin
  one-shot-tally status [--json]  show the latest run and lifetime totals
  one-shot-tally grade [--json]   alias for status
  one-shot-tally background record ID --cleanup CMD [--tmux-target PANE]
                                  record cleanup and the agent wake-up target
  one-shot-tally background complete ID
                                  mark complete and wake the originating tmux pane
  one-shot-tally background list  list recorded background jobs and cleanup commands
  one-shot-tally version          show the version
  one-shot-tally help|-h|--help   show this help
```

Run `one-shot-tally --help` for the installed binary's current help.

## Agent policy

- Delegate independent, bounded, low-risk work to `spark_worker` subagents when the collaboration runtime supports them.
- Good Spark work includes targeted searches, small fixtures, focused documentation, formatting, and narrow mechanical edits with explicit acceptance criteria.
- Keep requirements, architecture, authorization, credentials, destructive or external actions, integration, and final acceptance with the primary agent.
- Spark calls cost 0.25 of a normal call for tool-pressure scoring. Test limits, verification requirements, safety gates, and correctness penalties are never discounted.
- Five test runs is the normal pacing guideline. More tests reduce the discipline score but remain allowed when correctness requires them.
- A blocked production attempt costs points but is recoverable after the current revision passes verification. The production action itself remains blocked until then.
- Prefer at most two useful subagents at first. Do not create delegation work merely to improve the score.
- Do not watch or repeatedly poll long-running work. Record it with `one-shot-tally background record ID --cleanup CMD`, arrange `one-shot-tally background complete ID` at job exit, and continue useful work or stop.
- `record` captures the current `$TMUX_PANE` by default. `complete` wakes that pane with a resume-and-cleanup message. Use `--tmux-target PANE` when the originating agent is in a different known pane.
- Successful records receive a small capped reward. Passive `wait`, polling, `watch`, `tail -f`, sleep loops, and repeated tmux status checks reduce the score but remain allowed when genuinely necessary.
- Complete the requested outcome. Never optimize the tally by doing nothing, narrowing scope, or stopping at the first warning; spend the time necessary for evidence, implementation, and final verification.
- Preserve the complete delivery contract. Removing `ship-it`, `ship.sh`, `deploy-it`, Woodpecker deployment steps, or GitHub workflow entrypoints without an equivalent same-edit replacement is denied before anything changes. Continue immediately with a corrected contract-preserving edit. Successful correction plus final verification restores shipping eligibility, while the blocked attempt retains a score penalty.
- Treat PASS/FAIL as an outcome signal and the letter grade as process discipline. Failed tests at stop, an unverified edited revision, or an unresolved delivery-contract block receives F. A verified completed revision has a D/50 floor even if it was long or inefficient.

## Build and install

```sh
go test ./...
go build -o one-shot-tally .
install -m 0755 one-shot-tally "$HOME/.local/bin/one-shot-tally"
```

The hook reads a JSON event from standard input when invoked without a command. Persistent state defaults to `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for isolated tests or alternate storage.
