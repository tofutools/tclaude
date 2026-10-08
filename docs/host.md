# Host resource status

```bash
tclaude host status
tclaude host status --json
```

The command reads agentd's cached local snapshot through `GET /v1/host/status`.
It requires a running daemon. Agents need the ordinary `host.read` permission:

```bash
tclaude agent permissions grant <agent> host.read
```

This permission is not default-granted: the snapshot includes local configured
work-directory paths. Human operators can read it directly. It does not grant
filesystem access, modify resources, or publish metrics to federation peers.

Agentd samples once at startup and every 15 seconds. Status reads never probe
the host or run subprocesses. JSON includes `observed_at`, `age_seconds`,
`sample_interval_seconds` and a `status` of `warming`, `current` or `stale`.
Before the first sample, status is `warming`; samples older than 45 seconds
are `stale`. Missing readings remain unavailable, with errors, rather than
being reported as zero capacity or zero load.

The snapshot contains:

- CPU logical cores available to the daemon and 1/5/15-minute system load
  averages. Load averages are not CPU utilisation percentages.
- RAM total and available bytes. Linux uses `MemAvailable`, with a marked
  estimate on older kernels. macOS available RAM is an estimate from free,
  inactive and speculative page lists using the reported page size; purgeable
  pages are not added because they overlap other lists. This is not macOS's
  memory-pressure measurement. See Apple's [vm_stat implementation](https://github.com/apple-oss-distributions/system_cmds/blob/main/vm_stat/vm_stat.c).
- Disk total and bytes available to an unprivileged writer for the data
  directory, home, active groups' default working directories, and optional
  extra work directories. Duplicate paths are collapsed within each kind.
  For a directory not yet created, `measured_path` identifies the nearest
  existing ancestor whose filesystem was sampled; nothing is created.
- Live tclaude session and managed agent counts. A live session needs a
  non-exited SQLite row and a live tmux session. Managed agents are unique
  active identities whose current conversation has such a session. Unmanaged
  tclaude wrapper sessions count as sessions, not as managed agents; other
  tmux sessions are ignored.

These are OS observations, not per-agent sandbox/cgroup limits or reservations.
A partial snapshot can be current while one individual metric is unavailable.
The implementation uses Linux `/proc`, macOS `sysctl`/`vm_stat`, and native
filesystem statistics, without cgo or additional dependencies.

## Warning thresholds

Configure `host` in `~/.tclaude/config.json`:

```json
{
  "host": {
    "warn_load_per_core": 1.5,
    "warn_ram_available_percent": 10,
    "warn_disk_available_percent": 10,
    "work_dirs": ["/srv/work"]
  }
}
```

Those are the default thresholds. A one-minute load per logical core above
1.5, available RAM below 10%, or available disk below 10% produces a warning
in text status and the JSON `warnings` array. Zero disables that warning.
Thresholds must be finite and nonnegative; percentages cannot exceed 100.
Extra work directories must use absolute paths. Changes take effect on the
next sample. Unknown metrics never produce an invented low-resource warning.
