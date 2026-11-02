#!/usr/bin/env bash
# Live diagnostics for CI's jstest shard, started in the background by the
# test-suite action on Linux.
#
# The shard's runner sometimes receives a shutdown signal partway through, and
# no other job does. go test buffers Node's output, so a killed run leaves
# nothing behind: this writes directly to the step log, which survives. The
# journal shows what stopped the runner; the samples show the machine and the
# suites running at the end (`node --test` gives each suite its own process).
set -u

interval="${JSTEST_WATCH_INTERVAL:-5}"

stop() {
	# Named pids only: a background script shares the step's process group,
	# so `kill 0` would take the test run down with it.
	# shellcheck disable=SC2046
	kill $(jobs -p) 2>/dev/null
	exit 0
}
trap stop TERM INT

sudo -n journalctl --follow --lines=0 --no-pager --output=short-precise 2>&1 &

while :; do
	mem="$(awk '/^MemAvailable:/ { a = $2 } /^SwapFree:/ { s = $2 }
		END { printf "memavail=%dMB swapfree=%dMB", a / 1024, s / 1024 }' /proc/meminfo)"
	disk="$(df -m --output=avail . | tail -n 1 | tr -d ' ')"
	echo "[jstest-watch] $(date -u +%T) load=$(cut -d' ' -f1 /proc/loadavg) $mem disk=${disk}MB procs=$(ps -e --no-headers | wc -l)"
	ps -eo rss=,pcpu=,etimes=,args= --sort=-rss | head -n 5 | cut -c1-120 | sed 's/^/[jstest-watch]   /'
	sleep "$interval" &
	wait $!
done
