package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/kasuganosora/thinkbot/util/idgen"
)

// A docker CLI process is not the parent of the processes inside its container.
// Killing the CLI alone leaves those processes running. Each invocation therefore
// has its own container process group and an unguessable control directory.
// The group leader stays a shell whose argv contains that directory, so a stale
// PID cannot cause cancellation to signal an unrelated, reused process group.
const dockerExecScript = `state=$1
command=$2
exec 3<&0
trap 'rm -rf -- "$state"' EXIT
setsid sh -c '
state=$1
command=$2
printf "%s\n" "$$" > "$state/pid" || exit 125
[ ! -f "$state/cancel" ] || exit 125
stdout=$(readlink /proc/$$/fd/1)
stderr=$(readlink /proc/$$/fd/2)
sh -c "$command" <&3 &
child=$!
wait "$child"
status=$?
# A shell can exit while a background descendant still holds our output pipes.
# Docker then keeps waiting for EOF. Keep the identifiable group leader alive
# for exactly that period, so cancellation can still stop those descendants.
# Jobs with all output explicitly redirected retain their detached semantics.
while :; do
  pending=
  for stat in /proc/[0-9]*/stat; do
    IFS= read -r info < "$stat" 2>/dev/null || continue
    fields=${info##*) }
    set -- $fields
    [ "$3" = "$$" ] && [ "$1" != Z ] || continue
    proc=${stat%/stat}
    [ "${proc##*/}" != "$$" ] || continue
    for fd in "$proc"/fd/*; do
      target=$(readlink "$fd" 2>/dev/null)
      if [ "$target" = "$stdout" ] || [ "$target" = "$stderr" ]; then pending=1; break; fi
    done
    [ -z "$pending" ] || break
  done
  [ -n "$pending" ] || break
  sleep 0.05
done
exit "$status"
' thinkbot-exec "$state" "$command" <&3 &
child=$!
wait "$child"
`

const dockerExecCancelScript = `state=$1
[ -d "$state" ] || exit 0
# Mark cancellation before looking for the PID. If docker exec has not started
# its shell yet, that shell will observe the mark and never run user code.
: > "$state/cancel" || { [ ! -d "$state" ]; exit $?; }
attempt=0
while [ "$attempt" -lt 40 ]; do
  [ -d "$state" ] || exit 0
  pid=$(cat "$state/pid" 2>/dev/null)
  case "$pid" in
    ''|*[!0-9]*) ;;
    *)
      # Match a complete argv element, not a substring or just a process name.
      if [ -r "/proc/$pid/cmdline" ] && tr '\000' '\n' < "/proc/$pid/cmdline" | grep -Fqx -- "$state"; then
        kill -KILL "-$pid" 2>/dev/null || { [ ! -d "/proc/$pid" ]; exit $?; }
      fi
      exit 0
      ;;
  esac
  attempt=$((attempt + 1))
  sleep 0.05
done
# No leader yet. Leave the cancellation mark in place for a delayed start.
`

// dockerExecCommand prepares cancellation before dispatching a command. Images
// without setsid fail before user code runs instead of silently weakening stop.
// The returned cancel must run before the host docker CLI is killed.
func dockerExecCommand(ctx context.Context, container, workDir, command string, interactive bool) (*exec.Cmd, func() error, error) {
	state := "/tmp/" + idgen.New("thinkbot-exec")
	prepare := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c",
		`command -v setsid >/dev/null || { echo 'sandbox cancellation requires setsid' >&2; exit 125; }; umask 077; mkdir -- "$1"`, "thinkbot-exec", state)
	if out, err := prepare.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("prepare cancellable docker exec: %w: %s", err, out)
	}
	args := []string{"exec", "-w", workDir}
	if interactive {
		args = append(args, "-i")
	}
	args = append(args, container, "sh", "-c", dockerExecScript, "thinkbot-exec", state, command)
	cmd := exec.CommandContext(ctx, "docker", args...)
	stop := func() error {
		// Request cancellation has already invalidated ctx. Cleanup needs its own
		// bounded context, and is scoped to this invocation rather than the bot.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cleanupCtx, "docker", "exec", container,
			"sh", "-c", dockerExecCancelScript, "thinkbot-cancel", state).CombinedOutput()
		if err != nil {
			return fmt.Errorf("could not confirm container command cancellation: %w: %s", err, out)
		}
		return nil
	}
	return cmd, stop, nil
}
