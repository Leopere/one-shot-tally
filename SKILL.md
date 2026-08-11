---
name: one-shot-tally
description: Run and interpret the one-shot-tally hook, including status, scoring, Spark delegation discounts, and installation.
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

## Build and install

```sh
go test ./...
go build -o one-shot-tally .
install -m 0755 one-shot-tally "$HOME/.local/bin/one-shot-tally"
```

The hook reads a JSON event from standard input when invoked without a command. Persistent state defaults to `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for isolated tests or alternate storage.
