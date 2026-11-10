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
	# sudo runs as root, out of reach of a plain kill. Named pids only: a
	# background script shares the step's process group, so `kill 0` would take
	# the test run down with it.
	sudo -n kill "$journal_pid" 2>/dev/null
	# shellcheck disable=SC2046
	kill $(jobs -p) 2>/dev/null
	exit 0
}
trap stop TERM INT

# Node's flags come before the suite path: without them, and with basenames,
# the cut leaves the suite's name.
trim() {
	sed -E 's/ --[^ ]+//g; s# /[^ ]*/# #g' | cut -c1-120 | sed 's/^/[jstest-watch]   /'
}

sudo -n journalctl --follow --lines=0 --no-pager --output=short-precise 2>&1 &
journal_pid=$!

while :; do
	mem="$(awk '/^MemAvailable:/ { a = $2 } /^SwapFree:/ { s = $2 }
		END { printf "memavail=%dMB swapfree=%dMB", a / 1024, s / 1024 }' /proc/meminfo)"
	disk="$(df -m --output=avail . | tail -n 1 | tr -d ' ')"
	echo "[jstest-watch] $(date -u +%T) load=$(cut -d' ' -f1 /proc/loadavg) $mem disk=${disk}MB procs=$(ps -e --no-headers | wc -l)"
	# Every suite, however small: a wedged one need not be a big one.
	ps -eo rss=,pcpu=,etimes=,args= --sort=-rss | grep -F '.test.mjs' | trim
	ps -eo rss=,pcpu=,etimes=,args= --sort=-rss | grep -vF '.test.mjs' | head -n 3 | trim
	sleep "$interval" &
	wait $!
done
