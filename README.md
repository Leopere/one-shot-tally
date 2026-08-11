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
one-shot-tally todo add 'Investigate another issue' --context 'Useful but outside the current goal'
one-shot-tally todo list
one-shot-tally todo done TODO_ID
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
- Removing a ship gate (`ship-it`, `ship.sh`) or automated deploy gate (`deploy-it`, Woodpecker, or GitHub workflows) without an equivalent same-edit replacement is denied before anything changes. Stopping there fails the run. A subsequent successful contract-preserving edit plus final verification recovers shipping eligibility, while the blocked shortcut retains a 15-point penalty.
- F is reserved for a failed or unverified final outcome. A completed revision with recorded passing verification cannot score below D/50; time, tool pressure, premature production attempts, inspection, and waiting can still reduce it to that floor.
- Useful discoveries outside the current goal can be parked in a durable TODO list with required deferral context. Up to three unique parked items earn two points each, but only after the current outcome is verified; completing side items gives no extra points during that run.

## Park rabbit holes without losing them

When investigation uncovers worthwhile work outside the current acceptance boundary, record it and return to the current goal:

```sh
one-shot-tally todo add \
  'Evaluate semantic detection of near-duplicate inspections' \
  --context 'Requires a calibrated corpus; not required for the current change'
one-shot-tally todo list
```

The list is durable across turns and repositories. Each item records its source directory and rejects duplicate text from the same source. Use `todo done ID` when a later task completes it. The reward is deliberately attached to verified completion of the current goal—not to completing the side quest—so parking a tangent is encouraged while pursuing it prematurely is not.

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
| Delivery-contract guard | Prevents agents from claiming efficiency by deleting ship/deploy gates | A permanent turn failure can scare an agent into stopping | Removal is denied before it changes the revision; same-edit migration or a corrected edit can recover, with a retained penalty |
| Success floor | Keeps PASS/FAIL about outcomes | A long successful run can accumulate enough process penalties to receive F | Verified completion floors discipline at D/50; unresolved correctness or safety failures remain F |
| Deferred-work reward | Preserves useful discoveries without expanding current scope | Agents could invent TODOs to farm points | Context is required, duplicates are rejected, reward is capped at six, and it applies only after current verification |

The tally does not reward or punish elapsed wall-clock time by itself. A time target would invite sleeping and passive polling. Session guidance instead makes the ordering explicit: completing the requested outcome and verifying it outrank tool-count efficiency, and warnings are instructions to change approach rather than stop working.

The score is intentionally heuristic. It should prompt a review, not replace code review, security analysis, test coverage, or engineering judgment. If people optimize the number instead of the outcome, remove the hook or adjust the policy rather than adding more scoring rules.

## Development

```sh
go test ./...
go build ./...
```

Contributions that reduce false positives, improve event compatibility, or validate the scoring model against real agent runs are welcome.
