#!/usr/bin/env bash
source "$(dirname "$0")/cli-common.sh"
say "A hub and two fresh tclaude nodes. The hub admin issues one invite per node."
show "hub " "tclaude-hub invite"
INV_A=$("$E" hub invite)
echo "$INV_A"
sleep 1
show "hub " "tclaude-hub invite"
INV_B=$("$E" hub invite)
echo "$INV_B"
sleep 1.5
say "Each node connects with its invite"
on a federation connect ws://127.0.0.1:18470 --invite "$INV_A" --name node-a
on b federation connect ws://127.0.0.1:18470 --invite "$INV_B" --name node-b
sleep 2
say "Node a sees node b, but does not trust it yet"
on a federation peers
say "Compare fingerprints out of band, then trust on both sides (both nodes are mine: unrestricted)"
on b federation identity
IDB=$("$E" run b federation identity 2>/dev/null | head -1)
IDA=$("$E" run a federation identity 2>/dev/null | head -1)
on a federation trust "$IDB" --level unrestricted --yes --label b
on b federation trust "$IDA" --level unrestricted --yes --label a
on a federation status
sleep 2
