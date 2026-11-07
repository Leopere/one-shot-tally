#!/usr/bin/env python3
"""Submit or observe the exact pushed CLI revision; Jenkins owns installation."""
import os
from pathlib import Path
import re
import subprocess
import sys


def pushed_main_revision(root):
    def git(*args, timeout=10):
        return subprocess.check_output(["git", "-C", str(root), *args], text=True, timeout=timeout).strip()

    branch = git("symbolic-ref", "--short", "HEAD")
    if branch != "main":
        raise SystemExit("Jenkins delivery requires the main branch")
    revision = git("rev-parse", "HEAD")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise SystemExit("Could not determine the full main revision")
    remote = git("ls-remote", "--exit-code", "origin", "refs/heads/main", timeout=30)
    if remote.split() != [revision, "refs/heads/main"]:
        raise SystemExit("Local HEAD does not match origin/main")
    return branch, revision


def main():
    root = Path(__file__).resolve().parents[1]
    branch, revision = pushed_main_revision(root)
    controller = Path.home() / ".local/share/jenkins-local/one-shot-tally-delivery.py"
    if not controller.is_file():
        raise SystemExit("Jenkins tally delivery is not installed: " + str(controller))
    environment = os.environ.copy()
    environment.update({
        "DEPLOY_IT_COMMIT": revision,
        "DEPLOY_IT_BRANCH": branch,
        "DEPLOY_IT_ENVIRONMENT": "local-mac",
        "DEPLOY_IT_REPO_ROOT": str(root),
    })
    result = subprocess.run(
        [sys.executable, str(controller), "deploy", "--profile", "one-shot-tally", "--revision", revision],
        cwd=root, env=environment, timeout=295,
    )
    raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
