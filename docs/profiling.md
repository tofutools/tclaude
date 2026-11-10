# Profiling agentd

When `tclaude agentd` uses more CPU than it should, you can capture profiles of
the running daemon and see where the time goes. There are two ways to start a
capture, and both save to the same store, `~/.tclaude/data/profiles/`.

## From the dashboard

Open **Diagnostics › Performance** and click **⏱ Profile daemon…**. The dialog
opens with sensible defaults (a 30-second CPU profile and a 5-second execution
trace), so you can just press Enter. The daemon profiles itself while it keeps
running normally. You don't need any startup flag, and this works anywhere the
dashboard does, including over [remote access](remote.md).

Each capture appears in the list with its headline numbers:

| Column | Meaning |
|---|---|
| agentd CPU | agentd's own CPU time over the window, as `top` shows it (100% = one core) |
| child CPU | CPU used by child processes (tmux, git, …) that exited during the window. `top` lists these under their own names, not as agentd |
| subproc | Instrumented subprocess starts per second (currently every tmux call) |

The **Open** links show the capture in pprof's own web UI in a new tab: flame
graph, top table, graph, source, and so on, for the CPU, heap, allocs,
goroutine, block and mutex profiles. The daemon serves that UI behind
dashboard sign-in, so you don't need a Go toolchain, a second port, or an SSH
tunnel. **summary** opens the counter summary. **trace**, **stacks** and
**stats** download the execution trace, the full goroutine dump, and the
counter snapshot. **zip** downloads every file of the capture as one archive.
Only one capture can run at a time, and a running capture can't be deleted.

**share…** sends a finished capture to an agent. Pick the agent in the
search dialog, and the capture's files arrive in its inbox as message
attachments, with the summary inlined in the message body. The agent can then
read them with its own tools, for example `go tool pprof -top cpu.pprof`.
Files over the 25 MiB attachment limit (in practice only a long execution
trace) are left out, and the message says so.

The graph view needs Graphviz (`dot`) installed on the daemon's host. The flame
graph and top views work without it.

## From the command line

Start the daemon with the profiling endpoint enabled:

```bash
tclaude agentd serve --pprof
```

This serves Go's standard `net/http/pprof` endpoints and a tclaude counter
endpoint (`/debug/tclaude/stats`) on a private socket,
`~/.tclaude/data/agentd-pprof.sock`, with permissions 0600. The socket is
never in the agent-reachable `~/.tclaude/api/` tree. With `--pprof`, block and
mutex sampling also stay on for the daemon's lifetime.

Then capture:

```bash
tclaude agentd profile                       # 30s CPU + 5s trace
tclaude agentd profile --seconds 120 --label "12 agents idle"
tclaude agentd profile --out ./agentd-prof   # write somewhere else
```

The command prints a summary: CPU split, GC rate, goroutines, and a table of
subprocesses per second by command. When a Go toolchain is on `PATH` it adds
the `go tool pprof -top` output. Captures written to the default location also
appear in the dashboard list.

To attach `go tool pprof` directly, add a loopback TCP listener:

```bash
tclaude agentd serve --pprof-addr 127.0.0.1:6060
go tool pprof -http=: http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

`--pprof-addr` only accepts an explicit loopback host. To profile the daemon
from another machine, use the dashboard instead.

## Reading the results

- **High agentd CPU with a broad, flat CPU profile** usually means many
  background loops each doing a little work. The execution trace
  (`go tool trace trace.out`) shows how often goroutines wake up.
- **High child CPU or a high subprocess rate** points at polling through tmux
  or git. The summary's subprocess table names the commands.
- **High GC rate or allocation rate** shows up as `runtime.gcBgMarkWorker` and
  `runtime.mallocgc` in the CPU profile. Use the **allocs** profile to find
  where the allocations happen.

Captures stay until you delete them, from the dashboard or by removing their
directories under `~/.tclaude/data/profiles/`.
