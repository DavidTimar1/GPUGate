#!/usr/bin/env bash
# Build, start, stop and check GPUGate. Run it from anywhere: it works in its own folder.
#
# One binary, one port, one copy. This script never starts a second one, and the
# program itself refuses its port if something else already holds it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

binary="$here/dist/gpugate"
pidfile="$here/.state/gpugate.pid"
logfile="$here/.state/gpugate.out"
# The output of the run before the current one. Starting overwrites $logfile, and a crash's panic
# message is the last thing in it, so it is moved aside first: one previous copy is kept.
prevlogfile="$here/.state/gpugate.out.prev"

# running is true only when the pid file names a live process that is the gpugate binary. A pid
# file left behind by a crash can name a process that has since been given to something else,
# and stop must never signal that one.
running() {
  [ -f "$pidfile" ] || return 1
  local pid
  pid="$(cat "$pidfile")"
  [ -n "$pid" ] || return 1
  local comm
  comm="$(cat "/proc/$pid/comm" 2>/dev/null || ps -p "$pid" -o comm= 2>/dev/null || true)"
  [ "$(basename "${comm:-none}")" = "$(basename "$binary")" ]
}

# print_addresses reads the addresses the program itself bound to and logged into $logfile, and
# prints one "http://.../console" line per address. It never works an address out for itself:
# the program is the only thing that knows what it actually bound, so gate.sh only relays it.
print_addresses() {
  # Addresses other than loopback first: they are the ones a browser on another machine uses.
  # Loopback last. The port and the address form (IPv4 or IPv6) are whatever the program bound.
  # `|| true` here too: under `set -o pipefail` a grep with no match fails this assignment
  # and `set -e` then exits the script before the fallback below.
  lines="$(grep -o 'listening on http://[^ ]*/console' "$logfile" 2>/dev/null | sed 's/^listening on //' | sort -u || true)"
  [ -n "$lines" ] || { echo "see $logfile for the addresses it bound"; return; }
  # `|| true` on both: this script runs under `set -e`, so a grep that finds no line (the log
  # is a second old and the second address is not written yet) would exit the whole script
  # with 1 on a start that worked.
  echo "$lines" | grep -v -e '//127\.0\.0\.1:' -e '//\[::1\]:' || true
  echo "$lines" | grep -e '//127\.0\.0\.1:' -e '//\[::1\]:' || true
}

build() {
  echo "building..."
  mkdir -p "$here/dist"
  go build -o "$binary" ./cmd/gpugate
  echo "built $binary"
}

start() {
  if running; then
    echo "already running, pid $(cat "$pidfile")"
    print_addresses
    return 0
  fi
  # Always rebuild. `go build` is cached, so this costs almost nothing when nothing changed,
  # and it is the only way `start` cannot serve a binary older than the source in front of you.
  build
  mkdir -p "$here/.state"
  if [ -s "$logfile" ]; then
    mv -f "$logfile" "$prevlogfile"
  fi
  nohup "$binary" >"$logfile" 2>&1 &
  echo $! > "$pidfile"
  sleep 1
  if running; then
    echo "started, pid $(cat "$pidfile")"
    print_addresses
  else
    rm -f "$pidfile"
    echo "it did not start. what it said:"
    cat "$logfile"
    exit 1
  fi
}

stop() {
  if ! running; then
    if [ -f "$pidfile" ]; then
      echo "not running: the pid file was stale, removed"
    else
      echo "not running"
    fi
    rm -f "$pidfile"
    return 0
  fi
  pid="$(cat "$pidfile")"
  kill "$pid"
  for _ in $(seq 1 20); do
    running || break
    sleep 0.25
  done
  if running; then
    echo "it did not stop within 5 seconds, forcing it"
    kill -9 "$pid" || true
  fi
  rm -f "$pidfile"
  echo "stopped"
}

status() {
  if running; then
    echo "running, pid $(cat "$pidfile")"
    print_addresses
  else
    echo "not running"
    # A Go crash ends its output with "panic: ..." or "fatal error: ...", then goroutine stacks.
    # Only the last run's output is read: an older crash is in $prevlogfile and says nothing about
    # a run that was stopped on purpose.
    if [ -f "$logfile" ] && grep -q '^panic: \|^fatal error: ' "$logfile"; then
      echo "it crashed: the program's own output ends in a panic, see $logfile"
    fi
  fi
}

case "${1:-}" in
  build)   build ;;
  start)   start ;;
  stop)    stop ;;
  restart) stop; start ;;
  status)  status ;;
  log)     tail -f "$logfile" ;;
  run)     build; exec "$binary" ;;
  *)
    echo "usage: ./gate.sh {build|start|stop|restart|status|log|run}"
    echo
    echo "  build    compile the binary"
    echo "  start    build, then run it in the background"
    echo "  stop     stop it"
    echo "  restart  stop, then start (which builds)"
    echo "  status   say whether it is running"
    echo "  log      follow what it is printing"
    echo "  run      run it in this window instead of the background"
    exit 1 ;;
esac
