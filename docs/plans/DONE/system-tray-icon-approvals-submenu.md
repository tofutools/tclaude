# Tray icon: pending-approvals submenu

Shipped 2026-05.

The tray icon already flipped yellow when a `--ask-human` popup was
waiting, but the human had no fast way back to that popup once the
browser tab got buried. This slice surfaces the in-flight set
directly in the tray menu — one click reopens any pending approval.

## What shipped

### Submenu shape

Below the existing "Open dashboard / Reinstall skills / Open config"
rows, a disabled **"Pending approvals"** header followed by N
clickable slot rows. Hidden when nothing is pending; shown when
≥1 approval is in flight.

```
tclaude agentd
─────────────
Open dashboard
Reinstall agent skills
Open config.json
─────────────
Pending approvals          ← disabled header (or "+N more…" on overflow)
groups.spawn · alice · 24s ago     ← clickable; opens /approve/{id}
agent.clone · bob · 12s ago
─────────────
socket: …
popup:  …
─────────────
Quit
```

### Pre-allocated fixed slate

`fyne.io/systray` doesn't reliably support runtime add/remove of menu
items across platforms (Linux SNI vs macOS NSMenu vs Win32
Shell_NotifyIcon all have different quirks). So the design is:

- Create `trayApprovalSlotCount = 5` slots at onReady time, all
  initially `Hide()`d.
- On each 200ms poll tick, take a snapshot of pending approvals
  and rebind the slots — oldest-first so the longest-blocked
  popup lands at the top.
- Overflow (more pending than slots) surfaces as
  `Pending approvals (+N more…)` on the header.

### Stable click handlers via slot-binding

Each slot has its own goroutine on its `ClickedCh`. The goroutine
reads the slot's `currentID` at click time (NOT captured by closure
at wire time) — so the click fires on whatever approval is *currently*
shown, even if it's been rebound since onReady.

The slot's `id` field is mutex-protected so the poller (writer) and
click handler (reader) never race.

### Change detection

The poller already had `(lastPending, lastSudo, lastHint)` as a
change-detector to avoid unnecessary `SetIcon` / `SetTooltip` calls.
Extended to also track the **ID list** — two approvals can come and
go within a 200ms window leaving the count unchanged but the
identity set different. `sliceEq([]string, []string)` is the new
comparator (oldest-first stable ordering makes a simple
element-wise compare sufficient).

## Files

- `pkg/claude/agentd/popup.go`:
  - `pendingApprovalSummary` — tray-safe slice of an approval row.
  - `(*approvalRegistry).snapshot()` returns a `[]summary` sorted
    oldest-first.
- `pkg/claude/agentd/tray.go`:
  - `trayApprovalSlotCount` constant.
  - `approvalSlot` struct (item + mu + bound id).
  - `refreshApprovalsSubmenu(header, slots, summary)` — pure
    rebinder; the poller calls it.
  - `formatApprovalSlotLabel` — pure formatter for the row label.
  - `shortApprovalID`, `pendingIDs`, `sliceEq` — small helpers.
  - Poller loop tracks `lastIDs` alongside the existing counters.
  - Per-slot click goroutine spawned at onReady, reads
    `slot.getID()` at click time.

## Tests

`pkg/claude/agentd/tray_test.go`:

- `TestApprovalRegistry_SnapshotSortsOldestFirst` — pins the
  oldest-first ordering rule.
- `TestFormatApprovalSlotLabel_UsesConvTitleWhenPresent` — label
  carries the perm, conv title, age.
- `TestFormatApprovalSlotLabel_FallsBackToShortIDWhenNoTitle` —
  conv-id truncated to 8 chars when no title.
- `TestSliceEq` — equal / unequal / nil / different-length / order.

## Out of scope

- **Tray-mediated approve** (clicking the slot only opens the
  popup; pairing tray-click + popup-click as a 2-factor consent is
  a separate hardening pass — see `popup-transport-hardening.md`
  in the system-tray-icon TODO).
- **Slot count > 5**: tunable in code; left at 5 for now because
  the realistic blocked-popup count from a coordinated group rarely
  exceeds a handful.

## Cross-references

- `TODO/med-prio/system-tray-icon.md` — the parent follow-ups
  list. Updated in-place to mark this item shipped.
