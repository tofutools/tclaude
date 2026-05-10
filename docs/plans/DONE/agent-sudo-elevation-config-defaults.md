# `tclaude agent sudo` — config-driven defaults (v2 slice 1)

Shipped 2026-05.

V1 hardcoded the sudo cap, default duration, popup timeout, and
blocklist as Go constants. v2 promotes them into
`~/.tclaude/config.json` so a human curator can lower the cap without
recompiling, and per-conv overrides let one specific agent (e.g. a
trusted manager bot) get a longer window than the default.

The hardcoded values stay in the binary as fallbacks: an agent
without any config gets exactly the v1 behaviour.

## Config schema

`pkg/claude/common/config/config.go`:

```go
type AgentConfig struct {
    DefaultPermissions []string    `json:"default_permissions,omitempty"`
    Sudo               *SudoConfig `json:"sudo,omitempty"`
}

type SudoConfig struct {
    MaxDuration     string                          `json:"max_duration,omitempty"`
    DefaultDuration string                          `json:"default_duration,omitempty"`
    PopupTimeout    string                          `json:"popup_timeout,omitempty"`
    Blocklist       *[]string                       `json:"blocklist,omitempty"`
    Overrides       map[string]*SudoConfigOverride  `json:"overrides,omitempty"`
}

type SudoConfigOverride struct {
    MaxDuration     string    `json:"max_duration,omitempty"`
    DefaultDuration string    `json:"default_duration,omitempty"`
    PopupTimeout    string    `json:"popup_timeout,omitempty"`
    Blocklist       *[]string `json:"blocklist,omitempty"`
}
```

`Blocklist` is a pointer-to-slice so we can distinguish "field absent
→ keep the default block" from "field present, value `[]` →
explicitly empty blocklist". JSON null vs missing-key both decode to
nil; an empty array decodes to a non-nil pointer to an empty slice.

## Override matching

Same selector shape as the historical `permission_overrides
[<conv-id|prefix|title>]` pattern (the storage moved to SQLite long
ago, but the human-facing key shape is preserved here for parity).

`Config.MatchSudoOverride(convID, alias, title)`:

- Exact match: `key == convID || key == alias || key == title`.
- Conv-id prefix: `key` is ≥8 chars and is a prefix of `convID`.
  Same threshold `agent.ResolveSelector` uses.
- Title prefix: `key` is a prefix of `title` (so `"tclaude-dev-"`
  matches `"tclaude-dev-r-7"`).
- When multiple keys match, the longest key wins so a more specific
  override beats a generic prefix.

Returns `nil` when no key matches; the caller falls through to the
global `agent.sudo` block.

## Resolution path

`pkg/claude/agentd/sudo.go`:

```go
type resolvedSudo struct {
    MaxDuration     time.Duration
    DefaultDuration time.Duration
    PopupTimeout    time.Duration
    Blocklist       []string
}

func resolveSudoConfig(cfg *config.Config, convID, alias, title string) resolvedSudo
```

Three layers, in precedence order from low to high:

1. **Hardcoded defaults** — `sudoDefaultMaxDuration` (1h),
   `sudoDefaultDefaultDuration` (5m), `sudoDefaultPopupTimeout`
   (60s), `sudoDefaultBlocklist`
   (`[permissions.grant, permissions.revoke]`). Same numbers v1
   shipped.
2. **Global config** — `cfg.Agent.Sudo` fields, when non-empty.
   Bad duration strings are tolerated: a `time.ParseDuration` error
   preserves the previous layer's value.
3. **Per-conv override** — the row matched by
   `Config.MatchSudoOverride(convID, alias, title)`, if any.

`Blocklist` semantics are *replace*, not merge: a layer that sets it
fully replaces the previous list. An empty array means "no
blocklist" (the human opted out of the safety net knowingly).

`handleSudoRequest` calls `config.Load()` + `resolveSudoConfig` once
per request, so a config edit lands without restarting the daemon.

## Tests (3 new flow tests, on top of v1's 6)

`pkg/claude/agentd/sudo_flow_test.go`:

- `TestSudo_ConfigMaxDuration_LowersTheCap` — `agent.sudo.max_duration:
  "30m"` in config, request for `1h` → 400, error mentions `30m`,
  no rows.
- `TestSudo_PerConvOverrideMaxDuration_AppliedByTitle` — global
  `max_duration: "15m"`, override `manager-bot: { max_duration: "45m" }`,
  agent with title `manager-bot` requests `30m` → 200; another agent
  with title `plain-worker` requests `30m` → 400.
- `TestSudo_ConfigBlocklist_ReplacesDefaults` — config sets
  `blocklist: ["groups.own"]`. Request for `permissions.grant`
  (hardcoded-blocked in v1) now reaches the popup; request for
  `groups.own` (newly blocked) is refused without popup.

A small helper `writeSudoConfig(t, body)` drops the JSON under
`$HOME/.tclaude/config.json`. testharness.New seeds `$HOME` with a
per-test tmpdir via `t.Setenv`, so each test scopes its config
cleanly. **Order matters**: call `newFlow(t)` *before*
`writeSudoConfig` so the swap has happened first.

## Files

- `pkg/claude/common/config/config.go` — `SudoConfig`,
  `SudoConfigOverride`, `MatchSudoOverride`,
  `sudoOverrideKeyMatches`.
- `pkg/claude/agentd/sudo.go` — renamed consts to `sudoDefault*`,
  added `resolvedSudo`, `resolveSudoConfig`, `applySudoLayer`,
  `parseDurationOpt`. `handleSudoRequest` reads the resolved policy
  per request.
- `pkg/claude/agentd/sudo_flow_test.go` — `writeSudoConfig` helper +
  three new tests.

## Cross-references

- [`DONE/agent-sudo-elevation-v1.md`](agent-sudo-elevation-v1.md) — v1
  shape these layers extend.
- [`TODO/high-prio/agent-sudo-elevation.md`](../TODO/high-prio/agent-sudo-elevation.md)
  — remaining v2 slices: audit annotations, dashboard panel,
  tray-icon orange state.
