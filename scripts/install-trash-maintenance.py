#!/usr/bin/env python3
"""Prepare a native macOS maintenance job; activate only from the user's Terminal."""

import argparse
import os
from pathlib import Path
import plistlib
import pwd
import shutil
import subprocess
import sys
import tempfile

LABEL = "com.colinknapp.trash-maintenance"


def job(home):
    return {
        "Label": LABEL,
        "ProgramArguments": [str(home / ".local/libexec/trash-maintenance"), "--run"],
        "StartCalendarInterval": {"Weekday": 0, "Hour": 3, "Minute": 15},
        "RunAtLoad": True,
        "WatchPaths": [str(home / ".Trash")],
        "ThrottleInterval": 300,
        "ProcessType": "Background",
        "StandardOutPath": str(home / "Library/Logs/trash-maintenance.log"),
        "StandardErrorPath": str(home / "Library/Logs/trash-maintenance.error.log"),
        "Umask": 0o077,
    }


def prepare(destination, home):
    source = Path(__file__).resolve().parents[1] / "maintenance"
    destination.mkdir(parents=True, exist_ok=True)
    binary = destination / "trash-maintenance"
    subprocess.run(["xcrun", "swiftc", "-O", str(source / "TrashMaintenance.swift"),
                    str(source / "main.swift"), "-o", str(binary)], check=True)
    definition = destination / (LABEL + ".plist")
    definition.write_bytes(plistlib.dumps(job(home)))
    subprocess.run(["plutil", "-lint", str(definition)], check=True)
    subprocess.run([str(binary), "--help"], check=True)
    return binary, definition


def atomic_copy(source, target, mode):
    target.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".maintenance-install-", dir=target.parent)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(source.read_bytes())
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, target)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    choice = parser.add_mutually_exclusive_group(required=True)
    choice.add_argument("--prepare", type=Path, metavar="DIRECTORY")
    choice.add_argument("--install", action="store_true")
    choice.add_argument("--uninstall", action="store_true")
    args = parser.parse_args()
    if sys.platform != "darwin":
        parser.error("This job is for macOS")
    home = Path(pwd.getpwuid(os.getuid()).pw_dir)
    if args.prepare:
        prepare(args.prepare.resolve(), home)
        print("Prepared only. No job was installed, loaded, or run against Trash.")
        return
    if os.getuid() == 0:
        parser.error("Run as the logged-in user, without sudo")
    if os.environ.get("CODEX_THREAD_ID") or os.environ.get("CODEX_SANDBOX"):
        parser.error("Run installation from your normal macOS Terminal.")
    definition = home / "Library/LaunchAgents" / (LABEL + ".plist")
    target = home / ".local/libexec/trash-maintenance"
    domain = "gui/" + str(os.getuid())
    service = domain + "/" + LABEL
    if args.uninstall:
        result = subprocess.run(["launchctl", "bootout", service], capture_output=True, text=True)
        if result.returncode not in (0, 3, 113):
            raise RuntimeError("Could not unload maintenance job: " + result.stderr.strip())
        definition.unlink(missing_ok=True)
        target.unlink(missing_ok=True)
        print("Maintenance job removed. Existing logs were retained.")
        return
    prepared = Path(tempfile.mkdtemp(prefix="trash-maintenance-install-"))
    binary, staged_definition = prepare(prepared, home)
    # Finish the reviewed artifact before replacing or loading the live job.
    existing = subprocess.run(["launchctl", "print", service], capture_output=True)
    if existing.returncode == 0:
        subprocess.run(["launchctl", "bootout", service], check=True)
    atomic_copy(binary, target, 0o755)
    atomic_copy(staged_definition, definition, 0o600)
    (home / "Library/Logs").mkdir(parents=True, exist_ok=True)
    subprocess.run(["launchctl", "bootstrap", domain, str(definition)], check=True)
    subprocess.run(["launchctl", "print", service], check=True)
    print("Installed: weekly Sunday 03:15, login catch-up, and Trash-change triggers. No AI or API calls.")
    shutil.rmtree(prepared)


if __name__ == "__main__":
    main()
