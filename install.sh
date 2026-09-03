#!/bin/sh
set -eu

command -v sqlite3 >/dev/null 2>&1 || {
    echo "one-shot-tally: sqlite3 is required for goal history" >&2
    exit 1
}
sqlite3 -json :memory: 'select 1;' >/dev/null

install_home=${ONE_SHOT_INSTALL_HOME:-"$HOME"}
bin_dir="$install_home/.local/bin"
skill_dir="$install_home/.codex/skills/one-shot-tally"

mkdir -p "$bin_dir" "$skill_dir"
go build -o "$bin_dir/one-shot-tally" .
install -m 0644 SKILL.md "$skill_dir/SKILL.md"
cmp -s SKILL.md "$skill_dir/SKILL.md"
version_output=$("$bin_dir/one-shot-tally" version)
printf '%s\n' "$version_output"
printf '%s\n' "$version_output" | grep -Fq 'one-shot-tally 1.19.0 | ColinKnapp.com'
printf '%s\n' "one-shot-tally: production install verified"
