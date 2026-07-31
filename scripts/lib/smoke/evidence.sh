#!/usr/bin/env bash
# Evidence discipline for the filtered-proxy smokes.
#
# A smoke only counts as evidence if the named test ACTUALLY RAN AND PASSED.
# `go test` exits 0 for a skipped test, for a filter that matched nothing, and
# for a package with no test files, so an exit status is not evidence and never
# was. This file is the check that is.
#
# It is a separate, side-effect-free unit precisely so it can be self-tested
# without a sandbox — see selftest.sh, which run.sh executes before it trusts
# anything here.

# require_passed_tests LOG_FILE TEST_NAME...
#
# Fails unless every named test reports an explicit top-level pass in LOG_FILE.
# Returns 0 only when all of them did; prints every distinct reason it refused,
# rather than stopping at the first, so one CI run tells the whole story.
require_passed_tests() {
  local log="$1"
  shift
  local -a required=("$@")
  local failed=0

  if [[ ! -s "$log" ]]; then
    printf 'evidence: log %s is missing or empty; the smoke produced no output at all\n' "$log"
    return 1
  fi
  if [[ ${#required[@]} -eq 0 ]]; then
    # A flow that requires nothing would pass no matter what it did. That is
    # the vacuous shape this whole file exists to prevent, so it is an error
    # rather than a permissive default.
    printf 'evidence: no required test names were declared for %s\n' "$log"
    return 1
  fi

  local name pattern
  for name in "${required[@]}"; do
    # Escape regex metacharacters: a manifest name is data, and `.` or `+` in
    # one would otherwise match more (or less) than the test it names.
    pattern=$(printf '%s' "$name" | sed 's/[][\.^$*+?(){}|]/\\&/g')
    # Anchored to the start of the line and followed by a space: `--- PASS:`
    # lines for SUBTESTS are indented, and a bare prefix match would let
    # TestFoo be satisfied by TestFooBar.
    if ! grep -Eq "^--- PASS: ${pattern} " "$log"; then
      if grep -Eq "^--- SKIP: ${pattern} " "$log"; then
        printf 'evidence: %s SKIPPED; a gated-out smoke is not evidence\n' "$name"
      elif grep -Eq "^--- FAIL: ${pattern} " "$log"; then
        printf 'evidence: %s FAILED\n' "$name"
      else
        printf 'evidence: %s did not run at all (renamed, removed, filtered out, or build-tagged away)\n' "$name"
      fi
      failed=1
    fi
  done

  # `go test` prints "no test files" or "testing: warning: no tests to run"
  # while still exiting 0. Neither can produce a PASS line, so the loop above
  # already catches them — this is a clearer message for the common case.
  if grep -q 'no tests to run' "$log"; then
    printf 'evidence: the run reported "no tests to run"; the -run filter matched nothing\n'
    failed=1
  fi

  return "$failed"
}

# RESIDUAL, stated rather than implied: a parent test that reports a top-level
# pass while every one of its subtests skipped would satisfy this check. No
# current smoke has that shape — both harness smokes skip only at top level, and
# their t.Run bodies contain no skips — but nothing here would catch it if one
# were introduced. It is a review question, like a rename applied consistently
# to both the test and the manifest.
