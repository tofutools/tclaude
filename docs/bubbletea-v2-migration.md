# Bubbletea v2 Migration Plan

## Goal
Migrate from bubbletea v1 to v2 to get `ModShift` support (shift+enter for newlines in semantic search textarea).

## Files to migrate
Only 2 files use bubbletea/bubbles:
- `pkg/claude/conv/watch.go` (+ `watch_test.go`)
- `pkg/claude/session/watch.go`

## Module path changes

| v1 | v2 |
|----|-----|
| `github.com/charmbracelet/bubbletea` | `charm.land/bubbletea/v2` |
| `github.com/charmbracelet/bubbles/textinput` | `charm.land/bubbles/v2/textinput` |
| `github.com/charmbracelet/bubbles/textarea` | `charm.land/bubbles/v2/textarea` |
| `github.com/charmbracelet/bubbles/key` | `charm.land/bubbles/v2/key` |

## API changes to apply

### 1. View() return type
v1: `func (m *watchModel) View() string`
v2: `func (m *watchModel) View() tea.View`

Wrap all `return b.String()` with `return tea.View{Content: b.String()}`.

### 2. Key events: KeyMsg → KeyPressMsg
v1: `case tea.KeyMsg:`
v2: `case tea.KeyPressMsg:`

The `msg.String()` method still exists on v2's Key type, so `switch msg.String()` patterns should mostly work. Need to verify the exact string representations match (e.g., "enter", "esc", "ctrl+c", "alt+enter").

### 3. Shift+enter detection (the whole point)
After migration, replace the `case "alt+enter":` in semantic input handler with:
```go
case "shift+enter", "alt+enter":
```
Or check `msg.Mod` directly before the string switch:
```go
if msg.Mod.Contains(tea.ModShift) && msg.String() == "enter" {
    // insert newline
}
```
Need to test what `msg.String()` returns for shift+enter in v2 — it likely includes "shift+" prefix.

### 4. tea.Quit
v1: `return m, tea.Quit` (tea.Quit is a Cmd)
v2: `tea.Quit()` returns a `Msg`. Need to check exact usage pattern — likely `return m, tea.Quit` still works or becomes `return m, func() tea.Msg { return tea.QuitMsg{} }`.

Check v2 source for the exact Quit pattern.

### 5. Paste handling
v1: `msg.Paste` bool field on KeyMsg
v2: Separate `tea.PasteMsg` type with `.Content string`

In conv/watch.go, the old code had `if msg.Paste { ... }` inside KeyMsg handling. This was removed when we switched to bubbles components (they handle paste internally). But verify the bubbles v2 textarea/textinput handle paste correctly.

### 6. tea.EnterAltScreen
v1: `tea.EnterAltScreen` is a Cmd returned from Init()
v2: May have moved. Check — could be a View field (`View{AltScreen: true}`) or still a Cmd.

### 7. tea.WindowSizeMsg
Unchanged struct in v2 — `Width`, `Height` int fields. No changes needed.

### 8. tea.Tick
v1: `tea.Tick(duration, func(time.Time) tea.Msg)`
v2: Should be the same or similar. Verify.

### 9. tea.Batch
v1: `tea.Batch(cmds...)`
v2: Same signature. No changes needed.

### 10. Bubbles v2 component APIs
The textarea and textinput `Update()` methods return `(Model, tea.Cmd)` in both versions. The `View()` return type may change to `string` or `tea.View` — check.

Key binding creation with `key.NewBinding()` should be the same in bubbles v2.

## Migration steps

1. Create branch `bubbletea-v2`
2. Update go.mod: `go get charm.land/bubbletea/v2@latest charm.land/bubbles/v2@latest`
3. Remove old deps: `go get github.com/charmbracelet/bubbletea@none github.com/charmbracelet/bubbles@none`
4. Update imports in both watch.go files
5. Fix compilation errors mechanically (View return type, KeyMsg → KeyPressMsg, Quit, etc.)
6. Add shift+enter support in semantic input handler
7. Run `go build ./...` and fix remaining issues
8. Run `go test ./...`
9. Manual test: verify search, semantic search, worktree input, shift+enter all work
10. `go mod tidy`

## Risk areas
- **Key string format changes**: The `msg.String()` values like "enter", "esc", "ctrl+c", "up", "down" etc. must match between v1 and v2. If v2 changes any of these strings, the switch cases will silently stop matching. Mitigation: test all key combos manually.
- **lipgloss compatibility**: v2 may require a newer lipgloss. Check for breakage in styling code (used throughout both files).
- **Bubbles v2 View()**: If textarea/textinput View() returns `tea.View` instead of `string`, we'll need to extract `.Content` when embedding their output into our own View.
