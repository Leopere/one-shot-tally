# Automatic macOS Trash maintenance

This is a native background worker. It does not start an AI agent, send data to an API, or ask the model what to delete.

The prepared LaunchAgent runs on Sundays at 03:15, at login, and after changes to the current user's home Trash. The weekly calendar is the scheduling guarantee; change notifications are an extra trigger and can be coalesced. A sleeping Mac runs a missed calendar job when it wakes. A powered-off Mac catches up at login. Runs are throttled to one launch per five minutes, and a process lock prevents overlapping cleanup.

The worker permanently removes an item only when its native macOS date-added value is strictly more than seven days old. It checks the date and file identity again before deletion. A trashed directory is one item, including its contents. Creation and modification dates are not substitutes for time spent in Trash. Apple documents the date-added property's meaning and limitations in [URLResourceValues.addedToDirectoryDate](https://developer.apple.com/documentation/foundation/urlresourcevalues/addedtodirectorydate).

Unknown or invalid dates, future dates, top-level symlinks, and hard-linked regular files are skipped. Descendant symlinks are removed as links; their targets are never traversed. The worker refuses a symlink root and filesystem crossings. It only handles the logged-in user's home Trash. External-volume Trash and `.agent-recovery` storage are outside this job.

## Activate once from macOS Terminal

Run this once in your normal macOS Terminal, without sudo:

```sh
python3 /Users/aedev/dev/one-shot-tally/scripts/install-trash-maintenance.py --install
```

The installer builds and validates the executable and plist, installs the exact user LaunchAgent, and loads it with launchd. Subsequent runs need no AI input. It does not change Codex hooks, permission profiles, or Finder settings.

Check the native job and its dated JSON reports:

```sh
launchctl print gui/$(id -u)/com.colinknapp.trash-maintenance
tail -n 5 ~/Library/Logs/trash-maintenance.log
tail -n 5 ~/Library/Logs/trash-maintenance.error.log
```

Errors remain visible in the error log. Reports distinguish eligible, deleted, skipped, and failed items without recording filenames or contents. `removedEntries` counts actual removals, including descendants. `partiallyDeleted` records directories where some contents were removed before a failure; these are not counted as completed deletions.

To stop automatic cleanup:

```sh
python3 /Users/aedev/dev/one-shot-tally/scripts/install-trash-maintenance.py --uninstall
```

## Review and test without activation

```sh
python3 scripts/install-trash-maintenance.py --prepare /tmp/one-shot-maintenance-review
xcrun swiftc maintenance/TrashMaintenance.swift maintenance/Tests.swift -o /tmp/one-shot-maintenance-tests
/tmp/one-shot-maintenance-tests
SHIP_IT_KEEP_TEST_DIRS=1 python3 scripts/test-trash-maintenance-installer.py
```

Preparation compiles the binary, validates the plist, and runs only `--help`. Fixture tests use ordinary temporary directories and an injected clock. They never access actual Trash or load a LaunchAgent. Actual protected-directory cleanup remains a host acceptance check after activation.
