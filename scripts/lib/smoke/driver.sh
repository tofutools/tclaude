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
