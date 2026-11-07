#!/usr/bin/env python3
"""Guard against retired deploy-it commands becoming delivery evidence."""

import json
import os
import pathlib
import subprocess
import sys
import tempfile


def invoke(tally, event, cwd, env):
    result = subprocess.run([tally], input=json.dumps(event) + "\n", cwd=cwd,
                            env=env, text=True, capture_output=True)
    if result.returncode != 0:
        raise AssertionError(f"one-shot-tally exited {result.returncode}: {result.stderr}")
    lines = [line for line in result.stdout.splitlines() if line.strip()]
    if len(lines) != 1:
        raise AssertionError(f"expected one JSON line, got {result.stdout!r}")
    return json.loads(lines[0])


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: test-native-delivery.py ONE_SHOT_TALLY")
    tally = pathlib.Path(sys.argv[1]).resolve()
    if not tally.is_file() or not os.access(tally, os.X_OK):
        raise SystemExit(f"executable not found: {tally}")

    with tempfile.TemporaryDirectory(prefix="one-shot-retired-delivery-") as temp:
        root = pathlib.Path(temp)
        repo = root / "repo"
        state = root / "state"
        fake_bin = root / "bin"
        repo.mkdir()
        state.mkdir()
        fake_bin.mkdir()
        marker = root / "legacy-command-ran"
        retired = fake_bin / "deploy-it"
        retired.write_text(f"#!/bin/sh\nprintf invoked > {marker}\n", encoding="utf-8")
        retired.chmod(0o755)
        env = os.environ.copy()
        env.update({"ONE_SHOT_STATE_DIR": str(state), "PATH": str(fake_bin) + os.pathsep + env.get("PATH", "")})

        base = {"session_id": "retired-command", "turn_id": "legacy", "cwd": str(repo)}
        pre = invoke(tally, {**base, "hook_event_name": "PreToolUse", "tool_name": "Bash",
                             "tool_use_id": "legacy", "tool_input": {"command": "deploy-it production"}}, str(repo), env)
        if pre:
            raise AssertionError(f"retired command was annotated: {pre!r}")
        post = invoke(tally, {**base, "hook_event_name": "PostToolUse", "tool_name": "Bash",
                              "tool_use_id": "legacy", "tool_response": {"exit_code": 0}}, str(repo), env)
        if post:
            raise AssertionError(f"retired command supplied delivery evidence: {post!r}")
        if marker.exists():
            raise AssertionError("one-shot-tally executed the retired deploy-it binary")


if __name__ == "__main__":
    main()
