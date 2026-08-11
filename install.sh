#!/bin/sh
set -eu

install_home=${ONE_SHOT_INSTALL_HOME:-"$HOME"}
bin_dir="$install_home/.local/bin"
skill_dir="$install_home/.codex/skills/one-shot-tally"

mkdir -p "$bin_dir" "$skill_dir"
go build -o "$bin_dir/one-shot-tally" .
install -m 0644 SKILL.md "$skill_dir/SKILL.md"
"$bin_dir/one-shot-tally" version
