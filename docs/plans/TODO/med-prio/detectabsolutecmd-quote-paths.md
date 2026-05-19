# DetectAbsoluteCmd — quote paths containing spaces

`pkg/claude/common/binary.go`'s `DetectAbsoluteCmd` joins paths and
subcommands with bare spaces, no shell-quoting:

```go
func DetectAbsoluteCmd(subcommands ...string) string {
	args := append(DetectAbsoluteArgs(), subcommands...)
	return strings.Join(args, " ")
}
```

If `os.Executable()` returns a path with spaces — `/home/User Name/go/bin/tclaude`
or a macOS bundle path — the resulting shell command breaks at the
first space when handed to `sh -c <cmd>`. The user sees an obscure
"command not found" with the first word as the bogus binary name.

Surface area (callers that build shell commands from this):

- `pkg/claude/agentd/dir.go:368` — `openAttachCmd` (the spawn auto-focus
  / shell-attach payload).
- `pkg/claude/session/spawn_terminal_darwin.go:30` — macOS direct
  terminal-attach.
- `pkg/claude/session/focus_linux.go` — `buildLinuxAttachCmd` (the
  Linux focus fallback shipped in PR #201).

Right fix: shell-quote each argv element inside `DetectAbsoluteCmd`
itself, using the same `shellSingleQuote` shape the call sites already
have (kept private inside each package today — extracting to
`clcommon.ShellSingleQuote` is the natural pair). For Windows the
quoting rules differ (`"…"` with `\"` escapes); the existing call
sites all special-case Windows already, so the path-quoting fix should
follow suit.

History: surfaced as a cold-review nit on PR #201 (the Linux focus
fallback). Parked rather than bundled because the bug is cross-cutting
(three callers + a CLI helper) and the immediate user impact in PR
#201 was zero — the konsole-support feature's session IDs are UUIDs,
no spaces. Promote when the first user actually hits the
spaces-in-path case, or as a low-friction cleanup PR.

Relevant code:

- `pkg/claude/common/binary.go` — `DetectAbsoluteCmd`,
  `DetectAbsoluteArgs`.
- `pkg/claude/agentd/dir.go:336` — `shellSingleQuote` (the candidate
  helper to extract).
- `pkg/claude/session/focus_linux.go` — `linuxShellSingleQuote` (a
  second copy of the same helper — would also fold in).

No open questions; mechanical refactor.
