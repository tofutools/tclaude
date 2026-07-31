#!/usr/bin/env bash
# The evidence-discipline self-test, kept as an entry point of this shard.
#
# The checks themselves are SHARED — every smoke shard judges evidence with the
# same code, so it is proven in one place — but this path stays because it is
# the documented safe entry point for this directory, and because a shard that
# could not prove its own discipline would have to be trusted instead.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$here/../lib/smoke/selftest.sh" "$@"
