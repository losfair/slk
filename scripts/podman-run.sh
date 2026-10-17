#!/bin/sh
# Run slk in rootless podman. All state (config, tokens, SQLite cache)
# lives in the "slk-home" named volume, so it survives container restarts.
# First run: ./scripts/podman-run.sh --add-workspace --browser
set -eu

# Mount the API directory at the same absolute path on both sides, so api.json
# can be used by host clients too. Set local_api.socket_path to this directory's
# api.sock in the persisted config. SLK_API_DIR can override the host directory.
if [ -z "${SLK_API_DIR:-}" ]; then
	if [ -n "${XDG_RUNTIME_DIR:-}" ] &&
		[ -d "$XDG_RUNTIME_DIR" ] && [ ! -L "$XDG_RUNTIME_DIR" ] &&
		[ "$(stat -c '%u:%a' "$XDG_RUNTIME_DIR" 2>/dev/null || true)" = "$(id -u):700" ]; then
		SLK_API_DIR="$XDG_RUNTIME_DIR/slk"
	else
		SLK_API_DIR="${TMPDIR:-/tmp}/slk-$(id -u)"
	fi
fi
case "$SLK_API_DIR" in
	/*) ;;
	*) echo 'SLK_API_DIR must be an absolute path' >&2; exit 1 ;;
esac
if [ ! -e "$SLK_API_DIR" ]; then
	(umask 077; mkdir -p "$SLK_API_DIR")
fi
if [ -L "$SLK_API_DIR" ] ||
	[ "$(stat -c '%u:%a' "$SLK_API_DIR")" != "$(id -u):700" ]; then
	echo "API directory must be owned by you with mode 0700: $SLK_API_DIR" >&2
	exit 1
fi

exec podman run --rm -it \
	--name slk \
	-v slk-home:/home/slk \
	-v "$SLK_API_DIR:$SLK_API_DIR:Z" \
	-e TERM="${TERM:-xterm-256color}" \
	-e COLORTERM="${COLORTERM:-}" \
	-e TZ="${TZ:-$(cat /etc/timezone 2>/dev/null || echo UTC)}" \
	localhost/slk:latest "$@"
