# Discovery / state — open

## Selectable filtering in `conv ls -w`

Pressing `g` in the interactive table could open a fuzzy group
picker — filters the visible rows to convs that are members of the
selected group. The Groups COLUMN itself is shipped; this is the
*filter affordance* on top of it.

Reuse `pkg/claude/common/table` (the same interactive table that
backs `conv ls -w` and `session ls -w`) and the existing search
pipeline so the picker composes with text-search and `e` (archived
toggle).

Mnemonic: `g` = group picker.

## Files
- `pkg/claude/conv/list.go` — interactive table watch loop
- `pkg/claude/common/table` — table primitives
