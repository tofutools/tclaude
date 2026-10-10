# Sourced by the CLI demo scripts, which run under asciinema (see record.sh).
E=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/e2e.sh
BASE=$(realpath -m "${E2E_BASE:-${TMPDIR:-/tmp}/tce2e}")
say() { printf '\n\033[1;36m# %s\033[0m\n' "$*"; sleep 1.2; }
show() { printf '\033[1;32m%s$\033[0m %s\n' "$1" "$2"; sleep 0.8; }
on() { # on <a|b> <tclaude args...>: show the command, then run it as that node's operator
	local n=$1
	shift
	show "node-$n " "tclaude $*"
	"$E" run "$n" "$@" 2>&1
	sleep 1.6
}
