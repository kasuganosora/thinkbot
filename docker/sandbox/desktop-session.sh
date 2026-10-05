#!/bin/sh
# Minimal desktop session for this bot's shared X display.
# Idea (not a port): WeKnora starts a real DE on Xvfb so the RFB view is a
# usable desktop, not a black root. We keep it light: openbox + tint2 only.
# Idempotent. No VNC port. Scoped to $DISPLAY (default :99) via a pid file
# in /tmp, so another bot on the same host is not inspected.
set -eu
DISPLAY="${DISPLAY:-:99}"
export DISPLAY
export LANG="${LANG:-C.UTF-8}"
export LC_ALL="${LC_ALL:-C.UTF-8}"
export HOME="${HOME:-/root}"

num=$(printf '%s' "$DISPLAY" | tr -d ':' | cut -d. -f1)
case "$num" in
  ''|*[!0-9]*) echo "desktop-session: bad display $DISPLAY" >&2; exit 1 ;;
esac
sock="/tmp/.X11-unix/X${num}"
run="/tmp/thinkbot-desktop-${num}"
mkdir -p "$run" "$HOME"

i=0
while [ ! -S "$sock" ] && [ "$i" -lt 50 ]; do
  sleep 0.1
  i=$((i + 1))
done
if [ ! -S "$sock" ]; then
  echo "desktop-session: no X on $DISPLAY" >&2
  exit 1
fi

# pid file only — do not scan /proc for another display's window manager.
pid_alive() {
  [ -f "$1" ] || return 1
  pid=$(cat "$1" 2>/dev/null || true)
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

if command -v xsetroot >/dev/null 2>&1; then
  xsetroot -solid '#e8e8ea' 2>/dev/null || true
fi
if command -v xset >/dev/null 2>&1; then
  xset s off -dpms 2>/dev/null || true
fi

if ! command -v openbox >/dev/null 2>&1; then
  echo "desktop-session: openbox is not installed; rebuild the bot container so the image includes package openbox" >&2
  exit 0
fi

if command -v flock >/dev/null 2>&1; then
  exec 9>"$run/lock"
  flock -w 8 9 || exit 0
fi

if ! pid_alive "$run/openbox.pid"; then
  openbox >"$run/openbox.log" 2>&1 &
  echo $! > "$run/openbox.pid"
  i=0
  while ! pid_alive "$run/openbox.pid" && [ "$i" -lt 40 ]; do
    sleep 0.1
    i=$((i + 1))
  done
fi

if command -v xsetroot >/dev/null 2>&1; then
  xsetroot -solid '#e8e8ea' 2>/dev/null || true
fi

if command -v tint2 >/dev/null 2>&1 && ! pid_alive "$run/tint2.pid"; then
  cfg=/etc/thinkbot/tint2rc
  [ -f "$cfg" ] || cfg=
  if [ -n "$cfg" ]; then
    tint2 -c "$cfg" >"$run/tint2.log" 2>&1 &
  else
    tint2 >"$run/tint2.log" 2>&1 &
  fi
  echo $! > "$run/tint2.pid"
fi
exit 0
