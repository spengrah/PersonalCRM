#!/usr/bin/env bash
# port-pids.sh <tcp-port> — print the PIDs of the processes bound on <tcp-port>,
# one per line. Exit 0 if at least one PID was found, 1 otherwise, mirroring
# `lsof -ti tcp:<port>` so callers can both pipe to `xargs kill` and use the
# exit status as an is-port-bound test.
#
# lsof is absent on some dev hosts (e.g. minimal ARM containers), where a bare
# lsof invocation silently reports "nothing to kill". Fall back to ss, then
# fuser, then /proc/net/tcp, so the answer is real wherever the repo runs.
set -u

port="${1:?usage: port-pids.sh <tcp-port>}"

# De-dup, drop non-PID tokens, and set the found/none exit status.
emit() {
    printf '%s\n' "$@" | grep -E '^[0-9]+$' | sort -u | grep .
}

if command -v lsof >/dev/null 2>&1; then
    lsof -ti "tcp:$port"
    exit $?
fi

if command -v ss >/dev/null 2>&1; then
    # LISTEN rows carry users:(("proc",pid=123,fd=4),...) — take every pid=.
    pids=$(ss -ltnp "( sport = :$port )" 2>/dev/null | grep -o 'pid=[0-9]*' | cut -d= -f2)
    # shellcheck disable=SC2086
    emit $pids
    exit $?
fi

if command -v fuser >/dev/null 2>&1; then
    # PIDs go to stdout (space-separated); the "<port>/tcp:" label to stderr.
    pids=$(fuser "$port/tcp" 2>/dev/null)
    # shellcheck disable=SC2086
    emit $pids
    exit $?
fi

# Last resort: walk /proc ourselves. LISTEN sockets (state 0A) on the port give
# socket inodes; the owning PIDs are whoever holds an fd on one of those inodes.
if [ ! -r /proc/net/tcp ]; then
    echo "port-pids.sh: none of lsof/ss/fuser available and /proc/net/tcp unreadable" >&2
    exit 1
fi
port_hex=$(printf '%04X' "$port")
inodes=$(awk -v p="$port_hex" '$4 == "0A" && $2 ~ (":" p "$") {print $10}' \
    /proc/net/tcp /proc/net/tcp6 2>/dev/null)
[ -n "$inodes" ] || exit 1
# Flatten to one space-separated line so the case membership test below works.
# shellcheck disable=SC2086
inodes=" $(echo $inodes) "
pids=""
for piddir in /proc/[0-9]*; do
    for fdlink in "$piddir"/fd/*; do
        tgt=$(readlink "$fdlink" 2>/dev/null) || continue
        case "$tgt" in
        socket:\[*\])
            ino=${tgt#socket:[}
            ino=${ino%]}
            case "$inodes" in
            *" $ino "*)
                pids="$pids ${piddir#/proc/}"
                continue 2
                ;;
            esac
            ;;
        esac
    done
done
# shellcheck disable=SC2086
emit $pids
