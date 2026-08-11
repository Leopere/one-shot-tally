#!/bin/sh
set -eu
exec "${SHIP_IT_BIN:-$HOME/.local/bin/ship-it}" "$@"
