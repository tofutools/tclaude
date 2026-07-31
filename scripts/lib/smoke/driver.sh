#!/usr/bin/env bash
# The shared smoke DRIVER: manifest reading, drift guards, flow execution and
# evidence checking.
#
# Every smoke shard in this repo runs its flows through this one driver, so the
# anti-skip discipline is defined once and self-tested once. A shard that forked
# it would be a second place the rule "a skipped, renamed or zero-test smoke is
# a hard failure" could quietly weaken, and the whole point of the discipline is
# that it cannot weaken without a visible, reviewable diff.
#
# A shard's own entrypoint keeps exactly what is specific to it: which packages
# it installs, which tools it pins, and its flows. It calls:
#
#   smoke::load_manifest   "$here/manifest.txt" "$here/flows"  # + drift guards
#   smoke::run_flows       "$label"                            # execute + judge
#
# Both are safe to call without a sandbox; only the flows themselves are
# destructive, and smoke::load_manifest is what --validate-only stops after.
#
# Requires common.sh and evidence.sh to have been sourced already.

# SMOKE_REQUIRED_BY_FLOW maps a flow basename to the space-separated list of
# top-level test names its log must report an explicit pass for.
declare -A SMOKE_REQUIRED_BY_FLOW=()
# SMOKE_FLOW_FILES is the sorted list of flow files the manifest was validated
# against, so the runner cannot execute a different set than it checked.
declare -a SMOKE_FLOW_FILES=()

# smoke::load_manifest MANIFEST_FILE FLOWS_DIR
#
# Reads the manifest and refuses every state in which a downstream pass would be
# vacuous: an unparseable line, a flow that claims no tests, a flow with no
# manifest entry, a manifest entry naming a flow that does not exist, or no
# flows at all.
smoke::load_manifest() {
  local manifest="$1" flows_dir="$2"
  SMOKE_REQUIRED_BY_FLOW=()
  SMOKE_FLOW_FILES=()

  if [[ ! -f "$manifest" ]]; then
    smoke::error "no manifest at $manifest; a shard without one claims no evidence"
    return 1
  fi

  local flow names name
  # EVERY name on the line is recorded, not just the first. Reading the rest of
  # the line into a discarded variable would drop the second and later names
  # from the evidence set while every drift guard below still passed — silently
  # requiring less than the manifest says, which is the exact vacuous shape this
  # file exists to refuse.
  #
  # The `|| [[ -n ... ]]` tail matters for the same reason in the other
  # direction: read returns non-zero at EOF, so a final line with no trailing
  # newline would otherwise be skipped entirely.
  while read -r flow names || [[ -n "${flow:-}" ]]; do
    [[ -z "${flow:-}" || "$flow" == \#* ]] && continue
    if [[ -z "${names:-}" ]]; then
      smoke::error "manifest line for flow '$flow' names no test"
      return 1
    fi
    for name in $names; do
      [[ "$name" == \#* ]] && break
      SMOKE_REQUIRED_BY_FLOW["$flow"]+="$name "
    done
  done < "$manifest"

  mapfile -t SMOKE_FLOW_FILES < <(find "$flows_dir" -maxdepth 1 -name '*.sh' | sort)
  if [[ ${#SMOKE_FLOW_FILES[@]} -eq 0 ]]; then
    smoke::error "no flows found under $flows_dir"
    return 1
  fi

  # Every flow must be represented in the manifest, and every manifest entry
  # must correspond to a flow. Either kind of drift means a smoke is running
  # without recorded evidence, or evidence is claimed for a smoke that no longer
  # runs.
  local -A seen_flow=()
  local file name
  for file in "${SMOKE_FLOW_FILES[@]}"; do
    name="$(basename "$file" .sh)"
    seen_flow["$name"]=1
    if [[ -z "${SMOKE_REQUIRED_BY_FLOW[$name]:-}" ]]; then
      smoke::error "flow '$name' declares no required tests in $(basename "$manifest"); it could not fail"
      return 1
    fi
  done
  for name in "${!SMOKE_REQUIRED_BY_FLOW[@]}"; do
    if [[ -z "${seen_flow[$name]:-}" ]]; then
      smoke::error "manifest names flow '$name', but $flows_dir/$name.sh does not exist"
      return 1
    fi
  done
}

# --- Shard selection ---------------------------------------------------------
#
# A shard is a named subset of a manifest's flows that ONE CI job runs, plus the
# harnesses that job must install. Splitting the flows across jobs is how a smoke
# shard gets faster without dropping anything; the risk it introduces is that a
# flow quietly belongs to no shard and therefore never runs, while every job
# stays green. That is the same vacuous shape smoke::load_manifest refuses for a
# flow with no evidence, so it is refused the same way: mechanically, on every
# run, by a check that is itself self-tested.

# SMOKE_SHARD_NAMES is the declaration order of the shards, so messages and
# summaries read the way the file does.
declare -a SMOKE_SHARD_NAMES=()
# SMOKE_SHARD_FLOWS / SMOKE_SHARD_HARNESSES map a shard name to its
# space-separated flow and harness lists.
declare -A SMOKE_SHARD_FLOWS=()
declare -A SMOKE_SHARD_HARNESSES=()

# smoke::load_shards SHARDS_FILE
#
# Reads the shard map and refuses every state in which a shard split would drop
# coverage: an unparseable line, a shard missing either list, a shard naming a
# flow the manifest does not, a flow listed twice within one shard, and — the
# check this file exists for — a flow the manifest names that no shard runs.
#
# Requires smoke::load_manifest to have run: the manifest is what the union is
# checked against, and checking a shard map against nothing would pass vacuously.
smoke::load_shards() {
  local shards="$1"
  SMOKE_SHARD_NAMES=()
  SMOKE_SHARD_FLOWS=()
  SMOKE_SHARD_HARNESSES=()

  if [[ ${#SMOKE_REQUIRED_BY_FLOW[@]} -eq 0 ]]; then
    smoke::error "smoke::load_shards called before a manifest was loaded; the union check would prove nothing"
    return 1
  fi
  if [[ ! -f "$shards" ]]; then
    smoke::error "no shard map at $shards"
    return 1
  fi

  local shard key items item
  # Same `|| [[ -n ... ]]` tail as the manifest reader, for the same reason: a
  # final line with no trailing newline would otherwise be dropped silently, and
  # dropping a shard line is exactly what this file must never do quietly.
  while read -r shard key items || [[ -n "${shard:-}" ]]; do
    [[ -z "${shard:-}" || "$shard" == \#* ]] && continue
    if [[ ! "$shard" =~ ^[A-Za-z0-9._-]+$ ]]; then
      smoke::error "shard map names shard '$shard', which is not a plain name"
      return 1
    fi
    case "${key:-}" in
      flows|harnesses) ;;
      "")
        smoke::error "shard map line for '$shard' has no key; expected 'flows' or 'harnesses'"
        return 1
        ;;
      *)
        smoke::error "shard map line for '$shard' has unknown key '$key'; expected 'flows' or 'harnesses'"
        return 1
        ;;
    esac
    if [[ -z "${items:-}" ]]; then
      smoke::error "shard '$shard' declares an empty $key list"
      return 1
    fi

    # Deliberately no nameref: `local -n` needs bash 4.3, while everything else
    # here is satisfied by bash 4, and a shard map is small enough that two
    # explicit branches cost less than the version footgun.
    local existing
    if [[ "$key" == flows ]]; then
      existing="${SMOKE_SHARD_FLOWS[$shard]:-}"
    else
      existing="${SMOKE_SHARD_HARNESSES[$shard]:-}"
    fi
    if [[ -n "$existing" ]]; then
      smoke::error "shard '$shard' declares '$key' twice"
      return 1
    fi
    if [[ -z "${SMOKE_SHARD_FLOWS[$shard]:-}${SMOKE_SHARD_HARNESSES[$shard]:-}" ]]; then
      SMOKE_SHARD_NAMES+=("$shard")
    fi

    local collected=""
    for item in $items; do
      [[ "$item" == \#* ]] && break
      # Charset-gated before it is used. These names index associative arrays
      # and are compared with unquoted expansions, so a name containing `*`, `@`
      # or a glob character would either alias every key or expand against the
      # repo root this script cd's into. Refusing the name is cheaper than
      # quoting every use site correctly forever.
      if [[ ! "$item" =~ ^[A-Za-z0-9._-]+$ ]]; then
        smoke::error "shard '$shard' names $key entry '$item', which is not a plain name"
        return 1
      fi
      if [[ " $collected" == *" $item "* ]]; then
        smoke::error "shard '$shard' names $key entry '$item' twice"
        return 1
      fi
      if [[ "$key" == flows && -z "${SMOKE_REQUIRED_BY_FLOW[$item]:-}" ]]; then
        smoke::error "shard '$shard' names flow '$item', which the manifest does not"
        return 1
      fi
      collected+="$item "
    done
    if [[ -z "$collected" ]]; then
      # Every item was a trailing comment, so the line declares nothing.
      smoke::error "shard '$shard' declares an empty $key list"
      return 1
    fi
    if [[ "$key" == flows ]]; then
      SMOKE_SHARD_FLOWS["$shard"]="$collected"
    else
      SMOKE_SHARD_HARNESSES["$shard"]="$collected"
    fi
  done < "$shards"

  if [[ ${#SMOKE_SHARD_NAMES[@]} -eq 0 ]]; then
    smoke::error "shard map $shards declares no shards"
    return 1
  fi

  local name
  for name in "${SMOKE_SHARD_NAMES[@]}"; do
    if [[ -z "${SMOKE_SHARD_FLOWS[$name]:-}" ]]; then
      smoke::error "shard '$name' declares no flows"
      return 1
    fi
    if [[ -z "${SMOKE_SHARD_HARNESSES[$name]:-}" ]]; then
      smoke::error "shard '$name' declares no harnesses; a shard that installs nothing cannot launch anything"
      return 1
    fi
  done

  # THE UNION CHECK. Everything above is structural; this is the one that keeps
  # the split honest. A flow missing from every shard still has manifest
  # evidence, still has a flow file, and still passes every other guard — it
  # simply never runs, in any job, forever.
  local -a orphans=()
  local flow
  for flow in "${!SMOKE_REQUIRED_BY_FLOW[@]}"; do
    local covered=0
    for name in "${SMOKE_SHARD_NAMES[@]}"; do
      if [[ " ${SMOKE_SHARD_FLOWS[$name]}" == *" $flow "* ]]; then
        covered=1
        break
      fi
    done
    [[ "$covered" -eq 0 ]] && orphans+=("$flow")
  done
  if [[ ${#orphans[@]} -gt 0 ]]; then
    # Sorted so the message is stable: associative-array iteration order is not.
    local sorted
    sorted="$(printf '%s\n' "${orphans[@]}" | sort | tr '\n' ' ')"
    smoke::error "shard map $shards covers no shard for flow(s): ${sorted% }; every flow must run in at least one shard"
    return 1
  fi
}

# smoke::require_workflow_shards JOB_ID WORKFLOW_FILE...
#
# The union check above proves every flow belongs to a shard. It cannot prove
# that every shard is actually RUN, because the list of shards CI invokes lives
# in a workflow file — behind an operator-scope merge — while the shard map lives
# here. A shard added to shards.txt but not to the workflow matrix passes every
# other guard in this file and never executes, in any job, forever: the same
# vacuous green displaced one level up. This closes that.
#
# The dependency deliberately points from the repo INTO .github, never the other
# way: a script reading a workflow read-only keeps the job generic, whereas a
# workflow that had to know about flows would be the thing this whole layout
# exists to avoid.
#
# A workflow whose job does NOT pass --shard is skipped rather than checked. That
# is the unsharded shape — one job running every flow — which is complete
# coverage by construction, and it is also the state of the tree before the
# job-split workflow change lands.
smoke::require_workflow_shards() {
  local job="$1"
  shift
  local file block declared failed=0

  if [[ ${#SMOKE_SHARD_NAMES[@]} -eq 0 ]]; then
    smoke::error "smoke::require_workflow_shards called before a shard map was loaded"
    return 1
  fi

  local wanted
  wanted="$(printf '%s\n' "${SMOKE_SHARD_NAMES[@]}" | sort | tr '\n' ' ')"

  for file in "$@"; do
    if [[ ! -f "$file" ]]; then
      smoke::error "no workflow at $file; the shard-matrix check cannot prove anything about it"
      failed=1
      continue
    fi
    # The job block: from `  <job>:` to the next line at the same indent. Both
    # anchors are two-space-indented, which is the shape every job in these
    # workflows has.
    block="$(awk -v job="  $job:" '
      $0 == job { inblock = 1; next }
      inblock && /^  [^[:space:]#]/ { exit }
      inblock { print }
    ' "$file")"
    if [[ -z "$block" ]]; then
      # Not an error: a workflow that does not run this smoke at all has nothing
      # to agree with. Callers pass only files that should contain the job, and
      # a typo'd job id would show up as every file being skipped.
      continue
    fi
    if [[ "$block" != *"--shard"* ]]; then
      # Unsharded: one job runs every flow, so coverage is complete without a
      # matrix to agree with.
      continue
    fi

    # `shard: [a, b]` — the flow-sequence form. A matrix written in block form
    # is not parsed and must fail rather than be assumed to match: guessing here
    # would be exactly the silent pass this function exists to refuse.
    if ! grep -Eq '^ +shard: \[[^]]*\]$' <<< "$block"; then
      smoke::error "$file invokes $job with --shard but declares no 'shard: [...]' matrix this check can read"
      failed=1
      continue
    fi
    declared="$(grep -Eo '^ +shard: \[[^]]*\]$' <<< "$block" \
      | sed -E 's/^ +shard: \[//; s/\]$//; s/,/ /g' \
      | tr ' ' '\n' | grep -v '^$' | sort | tr '\n' ' ')"

    if [[ "$declared" != "$wanted" ]]; then
      smoke::error "$file runs $job with shards [${declared% }] but the shard map declares [${wanted% }]; a shard CI never invokes runs nowhere"
      failed=1
    fi
  done

  return "$failed"
}

# smoke::require_shard_harness_coverage KNOWN_HARNESS...
#
# The other half of the union check, for the thing a shard installs rather than
# the thing it runs. A harness the entrypoint can install but no shard claims
# would be installed by nobody once the split is on, and the flow needing it
# would fail with "not found" — a real failure, but one that reads like a broken
# smoke rather than a shard map with a hole in it. Refusing up front says which
# it is. Requires smoke::load_shards to have run.
smoke::require_shard_harness_coverage() {
  local -a known=("$@")
  if [[ ${#known[@]} -eq 0 ]]; then
    smoke::error "no installable harnesses were declared; the shard harness check would prove nothing"
    return 1
  fi

  local claimed="" name harness
  for name in "${SMOKE_SHARD_NAMES[@]}"; do
    claimed+="${SMOKE_SHARD_HARNESSES[$name]}"
  done

  local failed=0
  for harness in "${known[@]}"; do
    if [[ " $claimed" != *" $harness "* ]]; then
      smoke::error "harness '$harness' is installable but belongs to no shard; it would never be installed"
      failed=1
    fi
  done
  # And the other direction: a shard naming a harness nothing can install is a
  # typo that would otherwise surface as a missing binary deep inside a flow.
  for harness in $claimed; do
    if [[ " ${known[*]} " != *" $harness "* ]]; then
      smoke::error "a shard names harness '$harness', which this entrypoint cannot install"
      failed=1
    fi
  done
  return "$failed"
}

# smoke::select_shard NAME
#
# Narrows SMOKE_FLOW_FILES to the named shard's flows and sets
# SMOKE_SELECTED_HARNESSES to the harnesses that shard installs. Selecting an
# unknown shard is a hard failure naming the ones that exist: a typo'd shard name
# that silently ran nothing would be a green job that proved nothing at all.
declare -a SMOKE_SELECTED_HARNESSES=()
smoke::select_shard() {
  local want="$1" name
  local found=0
  for name in "${SMOKE_SHARD_NAMES[@]}"; do
    if [[ "$name" == "$want" ]]; then
      found=1
      break
    fi
  done
  if [[ "$found" -ne 1 ]]; then
    smoke::error "unknown shard '$want'; known shards: ${SMOKE_SHARD_NAMES[*]}"
    return 1
  fi

  local -a keep=()
  local file base flow
  for flow in ${SMOKE_SHARD_FLOWS[$want]}; do
    for file in "${SMOKE_FLOW_FILES[@]}"; do
      base="$(basename "$file" .sh)"
      [[ "$base" == "$flow" ]] && keep+=("$file")
    done
  done
  # load_manifest already proved every manifest flow has a file and load_shards
  # proved every shard flow is in the manifest, so this cannot legitimately
  # happen — it is here so a future change that breaks that chain fails loudly
  # rather than running a shorter list than it selected.
  if [[ ${#keep[@]} -ne $(wc -w <<< "${SMOKE_SHARD_FLOWS[$want]}") ]]; then
    smoke::error "shard '$want' selected ${#keep[@]} flow files for flows: ${SMOKE_SHARD_FLOWS[$want]}"
    return 1
  fi

  mapfile -t SMOKE_FLOW_FILES < <(printf '%s\n' "${keep[@]}" | sort)
  read -r -a SMOKE_SELECTED_HARNESSES <<< "${SMOKE_SHARD_HARNESSES[$want]}"
}

# smoke::run_flows LABEL
#
# Runs each flow in its own subshell — so a flow's trap, cwd and variables
# cannot leak into the next — and judges its evidence. Returns non-zero if any
# flow failed or produced no evidence. Requires SMOKE_ARTIFACTS.
smoke::run_flows() {
  local label="$1"
  local overall=0
  local -A flow_status=()
  local file name log status evidence_output
  local -a required

  for file in "${SMOKE_FLOW_FILES[@]}"; do
    name="$(basename "$file" .sh)"
    log="$SMOKE_ARTIFACTS/$name.log"
    smoke::log "Running flow: $name"
    status=0
    (
      set -euo pipefail
      # shellcheck source=/dev/null
      source "$file"
      flow::run
    ) 2>&1 | tee "$log" || true
    # Both halves are inspected: PIPESTATUS[0] is the flow, [1] is tee. Reading
    # only the flow would let a failed tee — which means the log this run's
    # evidence is read from is incomplete — resolve to success.
    status="${PIPESTATUS[0]}"
    if [[ "${PIPESTATUS[1]:-0}" -ne 0 ]]; then
      smoke::error "could not write the flow log $log; its evidence is unreadable"
      status=1
    fi

    # The exit status and the evidence are checked SEPARATELY and both must
    # hold. `go test` exits 0 for a skip, and a flow can also die before
    # reaching its tests at all; neither is evidence.
    read -r -a required <<< "${SMOKE_REQUIRED_BY_FLOW[$name]}"
    evidence_output=""
    if ! evidence_output="$(require_passed_tests "$log" "${required[@]}" 2>&1)"; then
      status=1
    fi

    if [[ "$status" -ne 0 ]]; then
      overall=1
      flow_status["$name"]="FAILED"
      {
        printf '### %s flow `%s` did not complete\n\n' "$label" "$name"
        printf 'A skip, missing/renamed test, build-tag mismatch, or zero-test success is a hard failure.\n\n'
        if [[ -n "$evidence_output" ]]; then
          printf '```text\n%s\n```\n\n' "$evidence_output"
        fi
        if ( source "$file"; declare -F flow::describe >/dev/null ); then
          printf 'What this flow must show:\n\n```text\n'
          ( source "$file"; flow::describe )
          printf '```\n'
        fi
      } | smoke::summary
      smoke::error "$label flow '$name' did not report the evidence it must"
    else
      flow_status["$name"]="passed"
      if ( source "$file"; declare -F flow::report >/dev/null ); then
        {
          printf '### %s `%s`\n\n```text\n' "$label" "$name"
          ( source "$file"; flow::report "$log" )
          printf '```\n'
        } | smoke::summary
      fi
    fi
  done

  {
    printf '### %s evidence\n\n' "$label"
    printf '| flow | required tests | result |\n| -- | -- | -- |\n'
    for file in "${SMOKE_FLOW_FILES[@]}"; do
      name="$(basename "$file" .sh)"
      printf '| `%s` | `%s` | %s |\n' \
        "$name" "${SMOKE_REQUIRED_BY_FLOW[$name]% }" "${flow_status[$name]:-not run}"
    done
  } | smoke::summary

  return "$overall"
}
