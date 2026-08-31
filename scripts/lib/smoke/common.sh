#!/usr/bin/env bash
# Shared plumbing: logging, GitHub step-summary output, and hard failure.
#
# Everything here degrades to plain stdout when GITHUB_STEP_SUMMARY is unset,
# so the entrypoint behaves identically when a human runs it outside CI.

smoke::log() { printf '\n=== %s\n' "$*"; }

# smoke::summary writes an operator-facing block. CI renders it on the job
# page, which is the only place a failure explains itself to someone who was
# not watching the log scroll past.
smoke::summary() {
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    cat >> "$GITHUB_STEP_SUMMARY"
  else
    cat
  fi
}

# smoke::error emits a CI annotation and the same text on stderr.
smoke::error() {
  printf '::error::%s\n' "$*"
  printf 'ERROR: %s\n' "$*" >&2
}

# smoke::download_extract_tar_xz URL SHA256 DEST — fetch a pinned source
# archive with curl's bounded transport retries, verify its exact bytes, and
# extract its single top-level directory into DEST. The archive path avoids the
# passt Git server's consistently truncated smart-HTTP response on hosted
# runners while retaining reproducible source identity through SHA-256.
smoke::download_extract_tar_xz() {
  local url="$1"
  local want_sha="$2"
  local destination="$3"
  local archive="${destination}.tar.xz"

  if [[ -z "$url" || -z "$want_sha" || -z "$destination" || -e "$destination" ]]; then
    smoke::error "downloading a pinned archive requires URL, SHA-256, and a nonexistent destination: ${destination:-<empty>}"
    return 1
  fi
  curl --fail --silent --show-error --location \
    --retry 3 --retry-all-errors --retry-delay 2 \
    --output "$archive" "$url" || return 1
  local got_sha
  got_sha="$(sha256sum "$archive" | awk '{print $1}')" || return 1
  if [[ "$got_sha" != "$want_sha" ]]; then
    smoke::error "archive checksum mismatch: got $got_sha, wanted $want_sha"
    return 1
  fi
  mkdir -p "$destination" || return 1
  tar -xJf "$archive" --strip-components=1 -C "$destination"
}

# smoke::apt_source_is_microsoft_only FILE — classify an active deb822 source
# file without changing it. Source files are world-readable on supported
# Ubuntu images, so this deliberately stays plain awk: the shared self-test can
# exercise the exact classifier without sudo or a live apt tree.
smoke::apt_source_is_microsoft_only() {
  local source_file="$1"
  awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*URIs:[[:space:]]*/ {
      found = 1
      line = $0
      sub(/^[^:]*:[[:space:]]*/, "", line)
      count = split(line, uri, /[[:space:]]+/)
      for (i = 1; i <= count; i++) {
        if (uri[i] == "") {
          continue
        } else if (uri[i] ~ /^#/) {
          break
        } else if (tolower(uri[i]) ~ /packages[.]microsoft[.]com/) {
          microsoft = 1
        } else {
          mixed = 1
        }
      }
    }
    END { exit !(found && microsoft && !mixed) }
  ' "$source_file"
}

# smoke::apt_mirror_list_without_unreachable_azure FILE — copy a GitHub
# runner mirror list to stdout while omitting only the exact Ubuntu archive
# endpoint currently supplied as its priority-1 mirror. The runner's mirror
# method falls back per index object, so one unreachable mirror can consume a
# transport timeout for every Packages/Translation/Components object instead
# of failing over once for the update as a whole.
#
# Keep this transformation separate from the privileged file replacement so
# the shared self-test can prove its matching boundary without touching apt.
smoke::apt_mirror_list_without_unreachable_azure() {
  local mirror_file="$1"
  awk '
    $1 == "http://azure.archive.ubuntu.com/ubuntu/" { next }
    { print }
  ' "$mirror_file"
}

# smoke::run_bounded_apt_update — keep an unreachable runner mirror from
# occupying a smoke job until GitHub's six-hour ceiling. apt's transport
# timeout bounds an individual dead connection; the outer timeout also covers
# mirror-method stalls where apt keeps retrying or waiting without returning.
#
# Kept as a separate function so the shared self-test can prove the exact
# command without touching the host's apt state.
smoke::run_bounded_apt_update() {
  sudo -n timeout --kill-after=10s 180s \
    apt-get \
    -o Acquire::Retries=2 \
    -o Acquire::http::Timeout=15 \
    -o Acquire::https::Timeout=15 \
    update --quiet
}

# smoke::apt_update — update Ubuntu's package indexes without letting an
# unrelated third-party source take the host prerequisite down. GitHub-hosted
# Ubuntu images may carry Microsoft sources for preinstalled tools; those
# sources are not needed by any smoke here and have returned 403/unsigned
# failures independently of the Ubuntu archive.
#
# This is deliberately source-specific, not an apt error workaround. Active
# Microsoft `deb` lines are commented in place, while Microsoft-only deb822
# `.sources` files are moved out of apt's source-parts directory. A deb822 file
# mixing Microsoft and another vendor is left unchanged with a warning rather
# than disabling the other vendor too. The original files are copied/moved into
# RUNNER_TEMP for diagnostics; isolation is limited to GitHub Actions because
# that runner is ephemeral and no restore is needed.
smoke::apt_update() {
  local source_file source_name backup_file awk_status grep_status
  local mirror_file mirror_backup filtered_mirror_file
  local disabled_count=0 mixed_count=0
  local backup_dir="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/tclaude-disabled-apt-sources"
  local microsoft_list_re='^[[:space:]]*deb(-src)?[[:space:]].*packages[.]microsoft[.]com([/:[:space:]]|$)'
  local microsoft_uri_re='^[[:space:]]*URIs:[[:space:]].*packages[.]microsoft[.]com([/:[:space:]]|$)'
  local -a source_files=()

  if [[ -z "${GITHUB_ACTIONS:-}" ]]; then
    smoke::log "Not running under GitHub Actions; skipping Microsoft apt-source isolation"
    if ! smoke::run_bounded_apt_update; then
      smoke::error "apt-get update failed outside GitHub Actions"
      return 1
    fi
    return 0
  fi

  # GitHub's Ubuntu image routes the distro sources through a generated mirror
  # list whose first choice is azure.archive.ubuntu.com. When that endpoint is
  # unreachable, apt can fetch the InRelease files from archive.ubuntu.com and
  # then spend minutes retrying the Azure endpoint separately for the package
  # indexes. Remove only that image-provided entry on the ephemeral runner;
  # archive.ubuntu.com and security.ubuntu.com remain configured fallbacks.
  mirror_file=/etc/apt/apt-mirrors.txt
  if [[ -f "$mirror_file" ]]; then
    if [[ ! -d "$backup_dir" ]] && ! mkdir -p "$backup_dir"; then
      smoke::error "could not create apt-source diagnostics directory: $backup_dir"
      return 1
    fi
    mirror_backup="$backup_dir/apt-mirrors.txt.original"
    filtered_mirror_file="$backup_dir/apt-mirrors.txt.filtered"
    if ! smoke::apt_mirror_list_without_unreachable_azure "$mirror_file" > "$filtered_mirror_file"; then
      smoke::error "could not inspect GitHub runner apt mirror list: $mirror_file"
      return 1
    fi
    if ! cmp -s "$mirror_file" "$filtered_mirror_file"; then
      if ! grep -Eq '^[[:space:]]*https?://' "$filtered_mirror_file"; then
        smoke::error "refusing to remove the Azure apt mirror because no fallback mirror remains in: $mirror_file"
        return 1
      fi
      if [[ -e "$mirror_backup" ]]; then
        smoke::error "apt mirror-list diagnostics path already exists: $mirror_backup"
        return 1
      fi
      smoke::log "Disabling unreachable GitHub runner apt mirror: http://azure.archive.ubuntu.com/ubuntu/"
      if ! sudo cp -- "$mirror_file" "$mirror_backup"; then
        smoke::error "could not save GitHub runner apt mirror list before editing: $mirror_file"
        return 1
      fi
      if ! sudo cp -- "$filtered_mirror_file" "$mirror_file"; then
        smoke::error "could not disable unreachable GitHub runner apt mirror: $mirror_file"
        return 1
      fi
    fi
  fi

  if [[ -f /etc/apt/sources.list ]]; then
    source_files+=(/etc/apt/sources.list)
  fi
  if [[ -d /etc/apt/sources.list.d ]]; then
    while IFS= read -r -d '' source_file; do
      source_files+=("$source_file")
    done < <(
      find /etc/apt/sources.list.d -maxdepth 1 \
        \( -type f -o -type l \) \
        \( -name '*.list' -o -name '*.sources' \) -print0
    )
  fi

  for source_file in "${source_files[@]}"; do
    if [[ "$source_file" == *.sources ]]; then
      # A .sources file is a deb822 document: moving one that also contains an
      # Ubuntu/vendor URI would disable an unrelated source along with
      # Microsoft. Only move a file whose active URI tokens are all Microsoft;
      # mixed files are warned about and left visible to apt.
      if smoke::apt_source_is_microsoft_only "$source_file"; then
        :
      else
        awk_status=$?
        if (( awk_status > 1 )); then
          smoke::error "could not inspect apt source file: $source_file"
          return 1
        fi
        if sudo grep -Eiq "$microsoft_uri_re" "$source_file"; then
          printf '::warning title=Mixed Microsoft apt source::leaving mixed source file unchanged: %s (contains Microsoft and non-Microsoft URIs)\n' \
            "$source_file"
          smoke::log "Leaving mixed Microsoft apt source unchanged: $source_file; apt-get update remains fatal if it cannot use this source"
          mixed_count=$((mixed_count + 1))
        else
          grep_status=$?
          if (( grep_status > 1 )); then
            smoke::error "could not inspect apt source file: $source_file"
            return 1
          fi
        fi
        continue
      fi
    else
      if sudo grep -Eiq "$microsoft_list_re" "$source_file"; then
        :
      else
        grep_status=$?
        if (( grep_status > 1 )); then
          smoke::error "could not inspect apt source file: $source_file"
          return 1
        fi
        continue
      fi
    fi

    if [[ ! -d "$backup_dir" ]] && ! mkdir -p "$backup_dir"; then
      smoke::error "could not create apt-source diagnostics directory: $backup_dir"
      return 1
    fi
    source_name="$(basename "$source_file")"
    backup_file="$backup_dir/${disabled_count}-${source_name}"
    if [[ -e "$backup_file" ]]; then
      smoke::error "apt-source diagnostics path already exists: $backup_file"
      return 1
    fi

    smoke::log "Disabling unrelated Microsoft apt source: $source_file"
    if [[ "$source_file" == *.sources ]]; then
      sudo grep -Ein "$microsoft_uri_re" "$source_file" || true
      if ! sudo mv -- "$source_file" "$backup_file"; then
        smoke::error "could not disable Microsoft apt source: $source_file"
        return 1
      fi
    else
      sudo grep -Ein "$microsoft_list_re" "$source_file" || true
      if ! sudo cp -- "$source_file" "$backup_file"; then
        smoke::error "could not save apt source before editing: $source_file"
        return 1
      fi
      if ! sudo sed -E -i \
        '/^[[:space:]]*deb(-src)?[[:space:]].*packages[.]microsoft[.]com([\/:[:space:]]|$)/I s|^|# tclaude disabled unrelated Microsoft source: |' \
        "$source_file"; then
        smoke::error "could not disable Microsoft apt source entries in: $source_file"
        return 1
      fi
    fi
    disabled_count=$((disabled_count + 1))
  done

  if (( disabled_count == 0 && mixed_count == 0 )); then
    smoke::log "No packages.microsoft.com apt sources detected; configured sources remain unchanged"
  elif (( disabled_count == 0 )); then
    smoke::log "No Microsoft apt sources were disabled; $mixed_count mixed source file(s) remain visible to apt"
  else
    smoke::log "Disabled $disabled_count unrelated Microsoft apt source file(s); Ubuntu source and package failures remain fatal"
  fi
  smoke::log "Updating apt indexes for host prerequisites"
  if ! smoke::run_bounded_apt_update; then
    smoke::error "apt-get update failed or exceeded its 180-second deadline after Microsoft-source isolation; inspect the apt output above for the failing configured source"
    return 1
  fi
}

# smoke::require_command fails early and by name. A smoke that dies because a
# tool is absent must not be mistakable for a boundary that refused something.
smoke::require_command() {
  local missing=0 tool
  for tool in "$@"; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      smoke::error "required command not found: $tool"
      missing=1
    fi
  done
  return "$missing"
}

# smoke::kill_listener PID... — stop a fixture listener started as
# `sudo ... socat ... &`. Needs `pkill` (procps), which the shards assert in
# their prerequisite list rather than discovering here.
#
# `$!` there is the PID of the SUDO WRAPPER. Whether that is also the listener
# depends on how sudo was built and invoked: when it needs no pty and no I/O
# log it may exec the command in place, but when it forks, killing the wrapper
# leaves the real socat holding the bind. The next flow then fails on an
# address:port that is still answering from the previous one — a failure that
# reads like a policy result and is not one.
#
# One level of children is what the callers need, and that rests on an
# assumption worth naming: the intermediates in front of socat exec in place
# rather than fork. `sudo` may fork (which is the whole defect), while
# `ip netns exec` execs — iproute2 forks only in batch mode. A forking
# intermediate would silently reinstate the original bug.
#
# So the wrapper's children are signalled and then the wrapper, and every step
# tolerates an already-dead process: cleanup runs on the failure path where
# some of this never started. One level of children is enough for the bind —
# a `socat ...,fork` per-connection child is a grandchild and holds no
# listening socket.
smoke::kill_listener() {
  local pid
  for pid in "$@"; do
    [[ -n "${pid:-}" ]] || continue
    sudo pkill -TERM -P "$pid" 2>/dev/null || true
    sudo kill -TERM "$pid" 2>/dev/null || true
  done
}

# smoke::trap_cleanup FUNC — install FUNC as the cleanup for EXIT *and* for
# INT/TERM.
#
# What EXIT alone actually costs is worth stating precisely, because the
# obvious answer is wrong: bash DOES run an EXIT trap when a fatal signal kills
# the shell, so the teardown itself is not lost. What is lost is the FAILURE. A
# script interrupted with SIGINT and no INT trap exits 0, and smoke::run_flows
# judges a flow by PIPESTATUS[0] — so a cancelled or Ctrl-C'd flow scores as a
# pass on the status axis, and only the evidence check stands between that and
# a green run. These handlers re-raise as an exit status, so an interrupted
# flow FAILS.
#
# GitHub sends SIGINT first when a job is cancelled, which is exactly the
# signal whose default leaves the status at 0.
#
# FUNC is attempted at most once per shell: the signal handlers exit, which
# fires the EXIT trap as well, and a fixture teardown running twice would
# report spurious errors from the second pass. "Attempted", not "completed" — a
# teardown interrupted partway by a second signal is not retried.
smoke::trap_cleanup() {
  local fn="$1"
  # The name is interpolated into three trap strings, so it is checked before
  # it is installed: this refuses anything that is not already a function,
  # which both keeps the interpolation structurally safe and surfaces a typo'd
  # cleanup name HERE rather than as "command not found" during teardown.
  if ! declare -F "$fn" >/dev/null; then
    smoke::error "smoke::trap_cleanup: $fn is not a function"
    return 1
  fi
  # shellcheck disable=SC2064  # fn must expand now: the trap outlives this call.
  trap "smoke::run_cleanup_once $fn" EXIT
  # shellcheck disable=SC2064
  trap "smoke::run_cleanup_once $fn; exit 130" INT
  # shellcheck disable=SC2064
  trap "smoke::run_cleanup_once $fn; exit 143" TERM
}

smoke::run_cleanup_once() {
  [[ -z "${SMOKE_CLEANUP_RAN:-}" ]] || return 0
  SMOKE_CLEANUP_RAN=1
  "$1"
}
