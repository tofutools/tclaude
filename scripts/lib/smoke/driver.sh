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
# SMOKE_SHARD_FLOWS maps a shard name to its space-separated flow list, which is
# the one thing a shard actually declares. SMOKE_SHARD_HARNESSES is DERIVED by
# smoke::load_shards as the union of those flows' own declarations — see
# smoke::load_flow_harnesses — never read from the shard map.
declare -A SMOKE_SHARD_FLOWS=()
declare -A SMOKE_SHARD_HARNESSES=()

# SMOKE_FLOW_HARNESSES maps a flow basename to the space-separated list of
# harnesses that flow launches. A flow that launches none records the empty
# string, which is why every read must distinguish "declared empty" from "never
# loaded" with ${...+set} rather than :-.
declare -A SMOKE_FLOW_HARNESSES=()

# smoke::load_flow_harnesses
#
# Reads each flow's own harness declaration: a `flow::harnesses` function in the
# flow file printing the harness names that flow launches, or the single token
# `none`.
#
# The declaration lives IN THE FLOW because that is the only place it cannot be
# left behind. When it lived in the shard map, moving a flow to another job
# without moving its harness satisfied every union check and then failed deep
# inside the flow with a "cannot install"-class error — a loud failure, but one
# that reads as a broken smoke rather than as a shard map with a hole in it.
# Declared here, the harness set travels with the file, and each shard's install
# set is derived rather than restated.
#
# `none` is REQUIRED rather than allowing an absent function or empty output: a
# flow that launches no harness and a flow whose author forgot to say are the two
# states that must not look alike, and only one of them is safe to derive from.
#
# Requires smoke::load_manifest to have run: SMOKE_FLOW_FILES is the set of flow
# files this reads, and reading an empty set would record nothing while passing.
#
# NOTE, because this is a NEW requirement on a flow file: reading a declaration
# means SOURCING the flow, including on the --validate-only path, which never
# sourced one before. A flow must therefore be inert at source time — function
# definitions and `set -euo pipefail`, nothing that touches a sandbox, a network
# or /etc/hosts. flow::run is where a flow is allowed to do things.
smoke::load_flow_harnesses() {
  SMOKE_FLOW_HARNESSES=()
  if [[ ${#SMOKE_FLOW_FILES[@]} -eq 0 ]]; then
    smoke::error "smoke::load_flow_harnesses called before a manifest was loaded; it would record no declaration at all"
    return 1
  fi

  local file name raw status verdict item collected
  local -a tokens=()
  for file in "${SMOKE_FLOW_FILES[@]}"; do
    name="$(basename "$file" .sh)"
    status=0
    # Sourced in a command-substitution subshell, exactly as smoke::run_flows
    # sources a flow to reach flow::describe and flow::report — so a flow's
    # `set -e`, traps and variables cannot reach the caller. The flow's own
    # top-level STDOUT is discarded; only what flow::harnesses prints is read.
    #
    # Its STDERR is deliberately NOT discarded. A flow with a syntax error is a
    # declaration problem, and swallowing bash's own diagnostic would leave this
    # function saying "could not be sourced" with no line number — which is the
    # same misattribution, one level down, that this whole change is about.
    #
    # NOT a mechanical enforcement of the inert-at-source-time contract, and the
    # difference matters: capturing `source`'s status at all requires putting it
    # in a `||` context, which suppresses `set -e` INSIDE the sourced file. A
    # flow whose top level runs a failing command therefore carries on and
    # returns 0. That contract is held by review, not by this reader.
    #
    # The outcome is carried on the FIRST LINE rather than in the exit status.
    # Reserved statuses would be indistinguishable from `flow::harnesses`
    # returning the same number itself: `flow::harnesses() { return 3; }` would
    # be reported as "declares no flow::harnesses", which is a lie about a
    # declaration that is right there in the file.
    raw="$(
      # shellcheck source=/dev/null
      source "$file" >/dev/null || { printf 'unsourceable\n'; exit 0; }
      declare -F flow::harnesses >/dev/null 2>&1 || { printf 'undeclared\n'; exit 0; }
      # BEFORE the call, so a non-zero status can be attributed. `declared` on
      # the wire means the declaration was reached; its absence means the
      # subshell died earlier.
      printf 'declared\n'
      flow::harnesses
    )" || status=$?
    # Verdict FIRST, then attribution. A bare `status != 0` check would blame the
    # declaration for a flow whose TOP LEVEL exited non-zero — `exit 5` beside a
    # perfectly good flow::harnesses reported as "flow::harnesses failed with
    # status 5" is the same lie about a declaration sitting in the file, one
    # trigger over. The marker is what discriminates, so read it first.
    verdict="${raw%%$'\n'*}"
    raw="${raw#"$verdict"}"
    if [[ "$status" -ne 0 ]]; then
      if [[ "$verdict" == declared ]]; then
        smoke::error "flow '$name' flow::harnesses failed with status $status"
      else
        smoke::error "flow '$name' exited with status $status before its harness declaration could be read; a flow must be inert at source time"
      fi
      return 1
    fi
    case "$verdict" in
      declared) ;;
      undeclared)
        smoke::error "flow '$name' declares no flow::harnesses; say which harnesses it launches, or 'none'"
        return 1
        ;;
      unsourceable)
        # "any error above", not "the error above": `source` also returns
        # non-zero when a flow's last top-level command merely fails, and bash
        # prints no diagnostic for that. Promising an error that is not there
        # sends the reader hunting for nothing.
        smoke::error "flow '$name' could not be sourced to read its harness declaration; see any error above"
        return 1
        ;;
      *)
        # Neither marker survived, so the subshell died before printing one —
        # nothing about this flow's declaration is known, and guessing would be
        # the silent pass this function exists to refuse.
        smoke::error "flow '$name' produced no harness-declaration verdict; its declaration could not be read"
        return 1
        ;;
    esac

    # Split with `read -a` rather than an unquoted expansion. The tokens are
    # UNVALIDATED at this point — validating them is the next few lines — and an
    # unquoted `$raw` would pathname-expand a `*` against the repo root the
    # entrypoint cd's into, turning the one name the charset gate exists to
    # refuse into a list of real filenames that quietly passes it. Newlines are
    # folded first because `read` stops at the first one and flow::harnesses may
    # print a name per line.
    tokens=()
    read -r -a tokens <<< "$(tr '\n' ' ' <<< "$raw")" || true
    collected=""
    for item in ${tokens[@]+"${tokens[@]}"}; do
      # Same charset gate as the shard map's names, for the same reason: these
      # names are compared through unquoted expansions and are interpolated into
      # a `harnesses::install_$name` call, so a glob or `@` would alias
      # everything or expand against the repo root.
      if [[ ! "$item" =~ ^[A-Za-z0-9._-]+$ ]]; then
        smoke::error "flow '$name' declares harness '$item', which is not a plain name"
        return 1
      fi
      if [[ " $collected" == *" $item "* ]]; then
        smoke::error "flow '$name' declares harness '$item' twice"
        return 1
      fi
      collected+="$item "
    done
    if [[ -z "$collected" ]]; then
      smoke::error "flow '$name' declares an empty harness set; say 'none' if it launches no harness"
      return 1
    fi
    if [[ "$collected" == *" none "* || "$collected" == "none "* ]]; then
      if [[ "$collected" != "none " ]]; then
        # The COMPANIONS, not the whole set. Printing `collected` names `none`
        # as one of the things `none` was declared alongside, which is both
        # false and useless: what the reader has to reconcile is `none` against
        # the real harnesses, and those are the names to drop or keep. The list
        # was already in hand — a message that does not read what it has is the
        # misattribution this whole change is about, wearing a third hat.
        local others=""
        for item in ${tokens[@]+"${tokens[@]}"}; do
          [[ "$item" == none ]] && continue
          others+="$item "
        done
        smoke::error "flow '$name' declares 'none' alongside ${others% }; 'none' means no harness and cannot be combined"
        return 1
      fi
      collected=""
    fi
    SMOKE_FLOW_HARNESSES["$name"]="$collected"
  done
}

# smoke::load_shards SHARDS_FILE
#
# Reads the shard map and refuses every state in which a shard split would drop
# coverage: an unparseable line, a shard with no flow list, a shard naming a
# flow the manifest does not, a flow listed twice within one shard, and — the
# check this file exists for — a flow the manifest names that no shard runs.
#
# It also DERIVES each shard's install set as the union of its flows' harness
# declarations. A shard map cannot declare harnesses at all: the `harnesses` key
# is refused by name rather than ignored, because a stale line silently skipped
# would read as a declaration that had taken effect.
#
# Requires smoke::load_manifest and smoke::load_flow_harnesses to have run: the
# manifest is what the union is checked against, and the flow declarations are
# what the install sets are derived from. Checking a shard map against nothing,
# or deriving from nothing, would pass vacuously.
smoke::load_shards() {
  local shards="$1"
  SMOKE_SHARD_NAMES=()
  SMOKE_SHARD_FLOWS=()
  SMOKE_SHARD_HARNESSES=()

  if [[ ${#SMOKE_REQUIRED_BY_FLOW[@]} -eq 0 ]]; then
    smoke::error "smoke::load_shards called before a manifest was loaded; the union check would prove nothing"
    return 1
  fi
  if [[ ${#SMOKE_FLOW_HARNESSES[@]} -eq 0 ]]; then
    smoke::error "smoke::load_shards called before smoke::load_flow_harnesses; every shard would derive an empty install set"
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
      flows) ;;
      harnesses)
        # Named rather than folded into the unknown-key case: this key USED to
        # be the declaration site, so a map still carrying it is a stale edit
        # someone believes took effect, not a typo.
        smoke::error "shard map line for '$shard' declares 'harnesses'; harnesses are declared per flow (flow::harnesses) and each shard's set is derived as the union"
        return 1
        ;;
      "")
        smoke::error "shard map line for '$shard' has no key; expected 'flows'"
        return 1
        ;;
      *)
        smoke::error "shard map line for '$shard' has unknown key '$key'; expected 'flows'"
        return 1
        ;;
    esac
    if [[ -z "${items:-}" ]]; then
      smoke::error "shard '$shard' declares an empty $key list"
      return 1
    fi

    if [[ -n "${SMOKE_SHARD_FLOWS[$shard]:-}" ]]; then
      smoke::error "shard '$shard' declares '$key' twice"
      return 1
    fi
    SMOKE_SHARD_NAMES+=("$shard")

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
      if [[ -z "${SMOKE_REQUIRED_BY_FLOW[$item]:-}" ]]; then
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
    SMOKE_SHARD_FLOWS["$shard"]="$collected"
  done < "$shards"

  if [[ ${#SMOKE_SHARD_NAMES[@]} -eq 0 ]]; then
    smoke::error "shard map $shards declares no shards"
    return 1
  fi

  # THE DERIVATION. A shard installs exactly the union of what its flows say
  # they launch, so moving a flow to another job moves its harnesses with it and
  # there is no second place to keep in step.
  #
  # An EMPTY derived set is legitimate and is not refused: a shard whose flows
  # all declare `none` genuinely installs nothing. The guard that used to sit
  # here — "a shard that installs nothing cannot launch anything" — was
  # protecting against a forgotten shard-map line, and the thing that can now be
  # forgotten is the flow's own declaration, which smoke::load_flow_harnesses
  # refuses by name. The protection moved with the declaration rather than
  # being dropped.
  local name flow harness derived
  for name in "${SMOKE_SHARD_NAMES[@]}"; do
    # A shard now enters SMOKE_SHARD_NAMES only through its `flows` line, and
    # that line's list was already proven non-empty above, so this cannot
    # legitimately happen. It is here so a future change that breaks that chain
    # fails loudly rather than deriving an empty install set for a job that
    # believes it runs something.
    if [[ -z "${SMOKE_SHARD_FLOWS[$name]:-}" ]]; then
      smoke::error "shard '$name' declares no flows"
      return 1
    fi
    derived=""
    for flow in ${SMOKE_SHARD_FLOWS[$name]}; do
      # ${...+set}, not :-, because "declared none" is the empty string and
      # must not be confused with a flow whose declaration was never read.
      if [[ -z "${SMOKE_FLOW_HARNESSES[$flow]+set}" ]]; then
        smoke::error "shard '$name' names flow '$flow', whose harness declaration was never loaded; its install set cannot be derived"
        return 1
      fi
      for harness in ${SMOKE_FLOW_HARNESSES[$flow]}; do
        [[ " $derived" == *" $harness "* ]] && continue
        derived+="$harness "
      done
    done
    SMOKE_SHARD_HARNESSES["$name"]="$derived"
  done

  # THE UNION CHECK. Everything above is structural; this is the one that keeps
  # the split honest. A flow missing from every shard still has manifest
  # evidence, still has a flow file, and still passes every other guard — it
  # simply never runs, in any job, forever.
  local -a orphans=()
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
# the thing it runs. A harness the entrypoint can install but no flow claims
# would be installed by nobody, and the flow needing it would fail with "not
# found" — a real failure, but one that reads like a broken smoke rather than a
# declaration with a hole in it. Refusing up front says which it is.
#
# It reads the DERIVED shard sets, so the check is unchanged in what it proves:
# a harness no shard installs is still a harness nothing installs. What moved is
# only where the sets come from. The mirror direction — a name nothing can
# install — names the FLOWS that declared it, because that is the file the fix
# belongs in.
#
# Requires smoke::load_shards to have run.
smoke::require_shard_harness_coverage() {
  local -a known=("$@")
  if [[ ${#known[@]} -eq 0 ]]; then
    smoke::error "no installable harnesses were declared; the shard harness check would prove nothing"
    return 1
  fi

  # Deduplicated across shards. Two shards legitimately claim the same harness
  # when two of their flows declare it, and iterating the raw concatenation would
  # report an uninstallable name once per claiming shard — the same defect
  # printed twice reads like two.
  local claimed="" name harness
  for name in "${SMOKE_SHARD_NAMES[@]}"; do
    for harness in ${SMOKE_SHARD_HARNESSES[$name]}; do
      [[ " $claimed" == *" $harness "* ]] && continue
      claimed+="$harness "
    done
  done

  local failed=0
  for harness in "${known[@]}"; do
    if [[ " $claimed" != *" $harness "* ]]; then
      smoke::error "harness '$harness' is installable but no flow declares it; it would never be installed"
      failed=1
    fi
  done
  # And the other direction: a harness nothing can install is a typo that would
  # otherwise surface as a missing binary deep inside a flow. The declaring
  # flows are named because the flow file is where the typo is.
  local flow declarers
  for harness in $claimed; do
    if [[ " ${known[*]} " != *" $harness "* ]]; then
      declarers=""
      for flow in "${!SMOKE_FLOW_HARNESSES[@]}"; do
        if [[ " ${SMOKE_FLOW_HARNESSES[$flow]}" == *" $harness "* ]]; then
          declarers+="$flow "
        fi
      done
      # Sorted: associative-array iteration order is not stable, and a guard
      # whose message reorders between runs reads like a different failure.
      if [[ -n "$declarers" ]]; then
        declarers="$(tr ' ' '\n' <<< "$declarers" | grep -v '^$' | sort | tr '\n' ' ')"
      fi
      smoke::error "flow(s) ${declarers:-<unknown> }declare harness '$harness', which this entrypoint cannot install"
      failed=1
    fi
  done
  return "$failed"
}

# smoke::select_shard NAME
#
# Narrows SMOKE_FLOW_FILES to the named shard's flows and sets
# SMOKE_SELECTED_HARNESSES to the harnesses that shard installs — the DERIVED
# union of its flows' own declarations, so a flow moved here brings its harnesses
# with it. Selecting an unknown shard is a hard failure naming the ones that
# exist: a typo'd shard name that silently ran nothing would be a green job that
# proved nothing at all. An EMPTY selection is legitimate: a shard whose flows
# all declare `none` installs nothing.
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
  # `read -a` ASSIGNS the array, so an empty here-string leaves it zero-length
  # rather than carrying a previous selection forward — a shard whose flows all
  # declare `none` selects nothing, which is what run.sh's length guard expects.
  # Verified, not assumed: `a=(one two three); read -r -a a <<< ""` leaves
  # ${#a[@]} at 0 and returns 0.
  #
  # Deliberately no preceding `SMOKE_SELECTED_HARNESSES=()` and no `|| true`.
  # Both looked like belt-and-braces and were neither: the assignment above
  # already clears, and a here-string always presents one line so this `read`
  # cannot return non-zero. Code that cannot fire, carrying a comment that says
  # what it protects against, is the same overclaim this file refuses elsewhere.
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
