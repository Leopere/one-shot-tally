# one-shot-tally

`one-shot-tally` is a Codex-compatible one-shot hook for outcome-first delivery.
It tracks tool use, verification state, background jobs, and durable TODOs.
It is coaching evidence, not a full quality grader.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). Licensed under [Creative Commons Attribution 4.0 International](LICENSE).

## Install

```sh
git clone https://github.com/Leopere/one-shot-tally.git
cd one-shot-tally
go test ./...
go build -o one-shot-tally .
install -m 0755 one-shot-tally "$HOME/.local/bin/one-shot-tally"
mkdir -p "$HOME/.codex/skills/one-shot-tally"
install -m 0644 SKILL.md "$HOME/.codex/skills/one-shot-tally/SKILL.md"
```

The repository binary and installed hook are separate files. A pull, build, tag, or release does not update `$HOME/.local/bin/one-shot-tally`.
After each upgrade, rerun `go build` and `install`. Confirm that `./one-shot-tally version` matches `$HOME/.local/bin/one-shot-tally version`.

Installation does not enable the hook.
Configure [Codex hooks](https://learn.chatgpt.com/docs/hooks) to run the absolute installed path for `SessionStart`, `PreToolUse`, `PostToolUse`, and `Stop`.
Use `/hooks` in Codex to review and trust that configuration.

The binary reads a JSON hook event from stdin. State defaults to `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for isolated evaluation.

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
one-shot-tally version
one-shot-tally help|-h|--help
```

## Outcome, verification, and grade

- PASS is a verified completed outcome.
- Grade is process discipline.
- `F` is for failed or unverified final outcome.
- A verified final outcome cannot fall below `D/50`.

The hook denies production and delivery commands until the active revision passes verification.
Removing contract-preserving delivery gates (`ship-it`, `ship.sh`, `deploy-it`, Woodpecker, GitHub workflow deploy entries) is denied before edits.
A blocked gate can recover only after a corrected same-edit replacement and final verification.

## Score model and limits

- Repeated identical calls, long inspection streaks, excessive test runs, and passive waits reduce score.
- Five test runs is a normal pacing guide, not a hard cap.
- Recognized Spark calls cost `0.25` pressure; verification and final checks are not discounted.
- A denied command does not count as an executed test.
- After final verification, blocked production attempts retain one capped penalty. A successful production call earns one capped credit.
- Command success does not prove service health.
- Warnings guide the agent. The related work signal changes the numeric score.

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

Contact ColinKnapp.com before you use this code or submit a contribution.
