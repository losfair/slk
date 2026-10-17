#!/bin/sh
# Run slk in rootless podman. All state (config, tokens, SQLite cache)
# lives in the "slk-home" named volume, so it survives container restarts.
# First run: ./scripts/podman-run.sh --add-workspace --browser
set -eu
exec podman run --rm -it \
	--name slk \
	-v slk-home:/home/slk \
	-e TERM="${TERM:-xterm-256color}" \
	-e COLORTERM="${COLORTERM:-}" \
	-e TZ="${TZ:-$(cat /etc/timezone 2>/dev/null || echo UTC)}" \
	localhost/slk:latest "$@"
