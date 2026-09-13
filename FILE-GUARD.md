# File removal protection

`./install.sh` installs `agent-file-guard` separately from tally and adds it to
the global Codex, Claude, and Cursor pre-execution hooks. Cursor also gets
shell, MCP, file-read, and Tab-read guards that deny access if the hook fails.
Existing hooks remain in place. Codex must trust the exact installed hook
definition before it runs; `/hooks` shows its status.

The guard blocks direct deletion tools, permanent shell deletion, common
Python and Node deletion calls, empty-Trash AppleScript, and explicit Trash
paths or path aliases. It blocks interactive terminal creation because later
terminal input does not receive another Codex pre-execution check.

To remove a file or folder while retaining a recovery copy:

```sh
agent-file-guard remove -- ./old-folder
agent-file-guard restore -- /absolute/path/.agent-recovery/ID/receipt.json
```

Removal atomically renames the item into adjacent `.agent-recovery` storage
on the same filesystem. The receipt records its original location. Restore
refuses to overwrite an existing destination. Recovery storage is excluded
from Git and has no automatic purge. It consumes disk space until a person
chooses what to retain. It is separate from macOS Trash.

The installer also selects the `agent-file-guard` Codex permission profile.
It retains broad development access and networking while denying reads and
writes to Trash paths, including volume Trash and case variants. It replaces
the legacy `danger-full-access` default. Configuration backups end in
`.before-agent-file-guard`.

Hooks alone cannot stop an opaque program, a specialized tool that skips
hooks, input to an existing terminal, Finder, or a command outside the agent.
Use the recovery command for intentional removals.

`config/trash-requirements.toml` is a prepared administrator-policy fragment,
not an installed system policy. An administrator must merge and validate it
against existing `/etc/codex/requirements.toml` policy before deployment.
Without a managed policy, a user can change their profile or disable a hook.
