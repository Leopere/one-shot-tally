# one-shot-tally

`one-shot-tally` is an experimental Codex-compatible hook that rewards outcome-first delivery: gather evidence, make a coherent change, verify the final revision, and stop looping.

It records tool calls, edit revisions, test outcomes, repeated work, verification state, and lifetime scores. It also encourages narrowly scoped `spark_worker` delegation by charging recognized Spark calls at one quarter of the normal tool-pressure cost.

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

The score is intentionally heuristic. It should prompt a review, not replace code review, security analysis, test coverage, or engineering judgment. If people optimize the number instead of the outcome, remove the hook or adjust the policy rather than adding more scoring rules.

## Development

```sh
go test ./...
go build ./...
```

Contributions that reduce false positives, improve event compatibility, or validate the scoring model against real agent runs are welcome.
