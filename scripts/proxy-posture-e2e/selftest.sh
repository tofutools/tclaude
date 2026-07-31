#!/usr/bin/env bash
# The evidence-discipline self-test, kept as an entry point of this shard.
#
# The checks themselves are SHARED with every other smoke shard, so the rule
# that a skipped, renamed or zero-test smoke is a hard failure is proven in one
# place and cannot differ between shards.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$here/../lib/smoke/selftest.sh" "$@"
