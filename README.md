# one-shot-tally

`one-shot-tally` is an experimental Codex-compatible hook that rewards outcome-first delivery: gather evidence, make a coherent change, verify the final revision, and stop looping.

It records tool calls, edit revisions, test outcomes, repeated work, verification state, background-job stewardship, and lifetime scores. It also encourages narrowly scoped `spark_worker` delegation by charging recognized Spark calls at one quarter of the normal tool-pressure cost.

Copyright © 2026 [ColinKnapp.com](https://colinknapp.com). Licensed under [Creative Commons Attribution 4.0 International](LICENSE).

## Why consider it

Agent workflows can drift into repeated searches, tiny edit cycles, and redundant test reruns. A mechanical counter makes that drift visible. The tool does not try to decide whether an implementation is good; it asks whether the final edited revision was actually tested and whether the path there was needlessly repetitive.

Use it as a coaching signal, not a universal measure of engineering quality.

## Install

```sh
git clone https://github.com/Leopere/one-shot-tally.git
cd one-shot-tally
go test ./...
go build -o one-shot-tally .
install -m 0755 one-shot-tally "$HOME/.local/bin/one-shot-tally"
```

Copy `SKILL.md` into your personal Codex skills directory if you want the accompanying operating guidance:

```sh
mkdir -p "$HOME/.codex/skills/one-shot-tally"
install -m 0644 SKILL.md "$HOME/.codex/skills/one-shot-tally/SKILL.md"
```

The binary consumes a compatible JSON hook event on standard input when invoked without arguments. Hook interfaces are experimental; confirm that your Codex environment supplies the event names and fields used in `main.go` before relying on enforcement.

## Commands

```text
one-shot-tally --help
one-shot-tally status
one-shot-tally status --json
one-shot-tally background record JOB_ID --cleanup 'tmux kill-session -t JOB_ID'
one-shot-tally background complete JOB_ID
one-shot-tally background list
one-shot-tally version
```

State defaults to `$HOME/.codex/state/one-shot-delivery`. Set `ONE_SHOT_STATE_DIR` for isolated evaluation.

## What it enforces

- A production-looking command is denied until the current edit revision has a passing test result.
- A final response is asked to include the mechanical score line.
- Repeated identical calls, long inspection streaks, excessive test runs, and redundant test duration reduce the score.
- Five test runs is a pacing guideline, not a hard safety ceiling. Required tests remain allowed.
- A blocked production attempt is penalized, but a later verified revision can recover from it.
- Recognized Spark calls cost `0.25` normal calls for tool-pressure scoring. Tests and correctness gates receive no discount.
- A successful background record earns back up to five points per job (ten total). Passive waits cost seven points each, capped at 25, so recording a job never fully erases a later polling penalty.
- Removing a ship gate (`ship-it`, `ship.sh`) or automated deploy gate (`deploy-it`, Woodpecker, or GitHub workflows) without an equivalent same-edit replacement is denied. The attempt permanently fails that turn and prevents it from shipping, even if tests later pass.

## Background work without polling

Before leaving a service, build, deployment watch, or other long job detached, create a durable record:

```sh
one-shot-tally background record docs-build \
  --cleanup 'tmux kill-session -t docs-build'
tmux new-session -d -s docs-build \
  'make docs; result=$?; one-shot-tally background complete docs-build; exit $result'
```

When run inside tmux, `record` captures `$TMUX_PANE`. `complete` marks the job complete and sends that pane a message telling the originating agent to resume the task and use the recorded cleanup command. Outside tmux, completion remains in the durable ledger and `background list` exposes it for cleanup. This removes the need for repeated `sleep`, `watch`, `tail -f`, tmux-status, or empty poll calls.

## Gaps and possible overcorrections

| Signal | Useful pressure | Failure mode | Current tuning |
|---|---|---|---|
| Test count | Discourages blind reruns | Complex changes legitimately need more than five suites | Advisory after five; additional runs lower the grade but are not blocked |
| Production gate | Prevents unverified pushes | A mistaken preflight can dominate the whole run | Command remains blocked; the score penalty is recoverable after final verification |
| Tool count | Discourages ceremony | A difficult task may need broad evidence | Penalty starts only after 30 weighted calls and is capped |
| Inspection streak | Encourages decisions | Read-heavy audits can look inefficient | Warning begins at eight; it does not block work |
| Spark discount | Makes bounded delegation cheaper | Labels can be gamed and delegation can add coordination overhead | Discount applies only to recognized Spark-tagged calls; final acceptance stays primary-owned |
| Regex classification | Keeps the hook small | Shell syntax, wrappers, and new tools can evade or confuse detection | Only command-like tools are classified; patch contents cannot forge command events |
| Passing command | Provides executable evidence | Exit code zero does not prove assertions ran or were meaningful | Documented limitation; teams should pair this with their real test contract |
| Background reward | Encourages cleanup and wake-up records | Agents could create records for trivial jobs to farm points | Bonus is capped at ten points and cannot bypass verification gates |
| Passive-wait penalty | Discourages polling long jobs | A bounded wait can occasionally be the least risky action | Warning and capped score penalty; never blocks the wait |
| Delivery-contract guard | Prevents agents from claiming efficiency by deleting ship/deploy gates | Delivery systems sometimes migrate legitimately | Uncompensated removal is denied and fails the turn; a same-edit equivalent replacement is allowed |

The score is intentionally heuristic. It should prompt a review, not replace code review, security analysis, test coverage, or engineering judgment. If people optimize the number instead of the outcome, remove the hook or adjust the policy rather than adding more scoring rules.

## Development

```sh
go test ./...
go build ./...
```

Contributions that reduce false positives, improve event compatibility, or validate the scoring model against real agent runs are welcome.
