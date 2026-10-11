#!/usr/bin/env bash
source "$(dirname "$0")/cli-common.sh"
set -eo pipefail
say "What does node b share with me? Agents are addressed as agt_…@b"
on a federation sessions b
R1=$("$E" run a federation sessions b 2>/dev/null | awk '/ reviewer-1 /{print $1}')
RH=$("$E" run a federation sessions b 2>/dev/null | awk '/ reviewer-home /{print $1}')
need R1 RH
say "Download a file from reviewer-1's project, read-only"
on a federation file get "$R1" notes.txt --output "$BASE/a/notes-from-b.txt"
show "node-a " "cat notes-from-b.txt"
cat "$BASE/a/notes-from-b.txt"
sleep 1.6
say "Guard rails: no parent traversal, no secrets, no agents rooted in a home directory"
on a federation file get "$R1" ../.claude.json --output "$BASE/a/x"
on a federation file get "$R1" .env --output "$BASE/a/y"
on a federation file get "$RH" proj/notes.txt --output "$BASE/a/z"
sleep 1
