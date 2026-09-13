# Move removals to Trash

The file guard rewrites ordinary shell `rm` calls to `agent-file-guard recycle`.
The replacement uses macOS Move to Trash and prints an undo command for each
successful move. The hook asks the agent to include those commands in its reply.

The shell still expands paths and globs. Quoted filenames, `--`, recursive
directories, multiple operands, and commands joined with `&&` keep their shell
structure. A shell parser distinguishes command calls from mentions in search
patterns or documentation. The supported flags are `-f`, `-r`, `-R`, `-d`, and
`-v`, plus their long forms where available. Unsupported flags return an error
before any operand is moved.

Each item gets a private receipt outside Trash. macOS chooses the correct Trash
destination, including filename collisions and external volumes. A symlink is
moved as a link. The helper does not follow its final target.

Example output:

```text
Moved to Trash: /path/to/old-build
Undo: '/Users/you/.local/libexec/agent-native-trash' restore --receipt '/Users/you/.local/state/agent-file-guard/receipts/ID.json'
```

Run the printed command in a host session that permits access to Trash. Restore
refuses to overwrite an existing path. A failed later move does not discard the
undo commands for earlier successful moves. A receipt-finalization failure is
reported as an uncertain result with recovery metadata; do not repeat that move.

Other deletion APIs, protected roots, and shell syntax that cannot be safely
rewritten retain their existing checks. Legacy Cursor shell hooks cannot rewrite
inputs; the installed `preToolUse` hook handles rewriting before shell execution.

The full installer builds the native helper and installs the guard hooks.
The tally-only installer leaves both components alone.

Run focused checks with:

```sh
SHIP_IT_KEEP_TEST_DIRS=1 go test ./internal/fileguard ./cmd/agent-file-guard
xcrun swiftc -parse-as-library -D TESTING native-trash/TrashCommand.swift native-trash/TrashCommandTests.swift -o /tmp/agent-native-trash-tests
/tmp/agent-native-trash-tests
```

The Swift tests use an isolated fixture mover. They do not inspect real Trash.
