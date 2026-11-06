// git-picker.js — the dashboard's choice of a git worktree or branch
// (tcl-2a1ihf). A big repo has hundreds of branches and dozens of worktrees,
// so every worktree and base-branch <select> is this type-to-filter choice
// instead (ChoiceCombobox, on FilterCombobox).
//
//   worktreeItems(worktrees, opts)  a repo's worktrees as FilterCombobox items
//   WorktreeCombobox                the choice of a worktree (or a new one)
//   BranchCombobox                  the choice of a branch (a new worktree's base)

import { h } from 'preact';
import { useMemo } from 'preact/hooks';
import htm from 'htm';
import { ChoiceCombobox } from './choice-combobox.js';
import { shortCwd } from './helpers.js';

const html = htm.bind(h);

// worktreeItems are a repo's worktrees, keyed `wt:<path>` and found by branch
// or path, then (newKey) the "+ create new worktree…" choice keyed newKey.
export function worktreeItems(worktrees = [], { newKey = '' } = {}) {
  const rows = worktrees.filter((worktree) => worktree?.path).map((worktree) => ({
    key: `wt:${worktree.path}`,
    label: `${worktree.branch || '(detached)'}${worktree.is_main ? ' [main]' : ''}`,
    hint: shortCwd(worktree.path),
    keywords: worktree.path,
  }));
  return newKey ? [...rows, { key: newKey, label: '+ create new worktree…', hint: '', keywords: 'new create' }] : rows;
}

// WorktreeCombobox chooses one of worktrees, or none ('' — what noneLabel
// says it means here), or newKey's new worktree when newKey is set.
export function WorktreeCombobox({ worktrees = [], newKey = '', noneLabel, ...props }) {
  const items = useMemo(() => worktreeItems(worktrees, { newKey }), [worktrees, newKey]);
  return html`<${ChoiceCombobox} placeholder="Type to find a worktree…" emptyHint="No worktrees." ariaLabel="Worktree"
    missing="no such worktree" ...${props} items=${items} noneLabel=${noneLabel} />`;
}

// BranchCombobox chooses one of branches (names). It cannot be cleared, as
// the <select> it replaces had no empty choice.
export function BranchCombobox({ branches = [], ...props }) {
  const items = useMemo(() => branches.filter(Boolean).map((name) => ({ key: name, label: name, hint: '' })), [branches]);
  return html`<${ChoiceCombobox} placeholder="Type to find a branch…" emptyHint="No branches." ariaLabel="Branch"
    missing="no such branch" ...${props} items=${items} />`;
}
