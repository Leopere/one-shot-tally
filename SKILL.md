---
name: one-shot-tally
description: Install, run, and interpret one-shot-tally for scoring, background jobs, Spark use, and durable TODO flow.
---
# One-Shot Tally

## Command help

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

## Rules

- Complete the requested outcome first.
- Final response must come from a verified edited revision.
- PASS marks verified completion. `F` marks a failed or unverified outcome. Grade is discipline.
- Delivery-gate edits are denied unless replaced in the same edit.
- A blocked gate can recover only after corrected edit and verification.
- Record successful verified production calls as delivery evidence. Do not treat command success as a live-service check.
- Use `spark_worker` for bounded, low-risk tasks.
- Spark has `0.25` pressure cost; gates and checks are never discounted.
- Before long jobs: `background record`; on exit: `background complete`.
- `complete` wakes the originating tmux pane with resume-and-cleanup guidance.
- Park out-of-scope work with `todo add TEXT --context WHY`; rewards apply only after current verification.
- Do not optimize the score at the cost of correctness.
- If tally bookkeeping fails, the hook reports the error and lets tool use continue.

## Build and install

```sh
go test ./...
./install.sh
```
The installed version line includes `ColinKnapp.com`.

Default state: `$HOME/.codex/state/one-shot-delivery`; set `ONE_SHOT_STATE_DIR` for alternate paths.
