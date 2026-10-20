#!/bin/sh
set -eu

if [ -f "${ONE_SHOT_INSTALL_HOME:-$HOME}/.config/one-shot-tally/disabled" ]; then
    printf '%s\n' "one-shot-tally is temporarily disabled; installation skipped."
    exit 0
fi

command -v sqlite3 >/dev/null 2>&1 || {
    echo "one-shot-tally: sqlite3 is required for goal history" >&2
    exit 1
}
sqlite3 -json :memory: 'select 1;' >/dev/null

install_mode=full
if [ "$#" -gt 0 ]; then
    if [ "$#" -eq 1 ] && [ "$1" = "--tally-only" ]; then
        install_mode=tally-only
    else
        echo "usage: ./install.sh [--tally-only]" >&2
        exit 2
    fi
fi

install_home=${ONE_SHOT_INSTALL_HOME:-"$HOME"}
bin_dir="$install_home/.local/bin"
skill_dir="$install_home/.codex/skills/one-shot-tally"

mkdir -p "$bin_dir" "$skill_dir"
go build -o "$bin_dir/one-shot-tally" .
install -m 0644 SKILL.md "$skill_dir/SKILL.md"
mkdir -p "$skill_dir/hooks"
install -m 0644 hooks/codex.json "$skill_dir/hooks/codex.json"
cmp -s SKILL.md "$skill_dir/SKILL.md"
cmp -s hooks/codex.json "$skill_dir/hooks/codex.json"
version_output=$("$bin_dir/one-shot-tally" version)
printf '%s\n' "$version_output"
printf '%s\n' "$version_output" | grep -Fq 'one-shot-tally 1.23.0 | ColinKnapp.com'

if [ "$install_mode" = tally-only ]; then
    printf '%s\n' "one-shot-tally: tally-only install verified"
    exit 0
fi

mkdir -p "$install_home/.local/libexec"
xcrun swiftc -parse-as-library -O native-trash/TrashCommand.swift -o "$install_home/.local/libexec/agent-native-trash"
go build -o "$bin_dir/agent-file-guard" ./cmd/agent-file-guard
"$bin_dir/agent-file-guard" install
python3 scripts/install-file-guard.py
printf '%s\n' "one-shot-tally: production install verified"
