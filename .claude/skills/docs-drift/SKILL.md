---
name: docs-drift
description: Compare docs/ with what the dashboard really shows, and file the differences as tickets for review. Use when asked to check the docs against the UI, or when a schedule fires this skill. It reads only, on a seeded throwaway server, and never edits docs, UI or code.
---

# Docs drift

Find places where `docs/` tells a reader something the dashboard does not do, and
file each one once, with evidence. A noisy report gets ignored: file only what you
would defend to the operator.

Never change docs, UI or code here. Never use `tclaude agent guide capture` or the
operator's dashboard: it shows them a notice and shows whatever is on their screen.
The seeded tour gives the same view with invented data.

## 1. Record the tour

```bash
scripts/docs/drift.sh            # add --images to also list stale docs images
```

Run it from a checkout of the commit under test. It writes
`docs-drift-out/tour/<stop>.txt` and `.png`, and `index.json`. A `.txt` holds the
visible text, then `--- controls ---`: every control label, marked `shown:` or
`hidden:` (in a closed menu or dialog). An `error` in `index.json` means a stop was
not reachable. That is a finding only if a doc promises that page or control; a
renamed selector in `tour.json` is a tour bug, so fix the tour.

## 2. Compare, one doc page at a time

Cover the pages about the dashboard: `docs/dashboard*.md`, `docs/fleet-ui.md`,
`docs/companion.md`, `docs/boards.md`, `docs/processes.md` and the dashboard parts of
`docs/learn/`. Use a subagent per few pages, so one context holds no more than it
can read closely. Read each doc beside the stops it describes, and the
`pkg/claude/agentd/dashboard/js` source when the tour cannot settle it.

A claim counts as drift only if all of these hold:

1. The doc states it as fact about the current UI: a tab, button, menu item, column,
   label, placement or behaviour.
2. The tour shows something else, or it is absent from the tour.
3. The source agrees. Search the dashboard JS for the doc's words. If they exist,
   find what shows them. Seeded data has no Plugins, no live terminal and no real
   host, so a thing that only appears with data the fixture lacks is not drift.

Do not report wording you merely prefer, sentence case against the UI's lower case,
punctuation such as a trailing `…`, emoji, or a claim you could not settle. Put
those in your report as unsettled, never in a ticket.

## 3. File

Write `docs-drift-out/findings.json`, an array of:

```json
{
  "title": "docs: dashboard-config says X, the UI shows Y",
  "doc": "docs/dashboard-config.md",
  "line": 12,
  "quote": "the doc's exact words, one sentence at most",
  "ui_shows": "what the tour shows, with the source file that settles it",
  "capture": "tour/config-agents.png",
  "fix": "the wording you propose, for a human to accept"
}
```

Check it, then file it:

```bash
scripts/docs/drift-file.sh docs-drift-out/findings.json             # new or known
scripts/docs/drift-file.sh --file -w <workspace> docs-drift-out/findings.json
```

The fingerprint is the doc path and the quote, so moved lines stay the same finding.
A finding with a fingerprint on any ticket, open or closed, is skipped: closing a
ticket as "not drift" silences it, and a fix to the doc retires it because the
quote is gone. Every ticket carries `docs-drift` and `needs-review`.

Report what was filed, what was known, what stayed unsettled and which stops failed.

## Running it on a schedule

This is opt-in; nothing runs until someone adds the job. `--action spawn` needs
`features.triggers`, and a worker keeps the build and the tour out of the Steward's
context. Use a spawn profile whose directory is a checkout of this repository.

```bash
tclaude agent cron add --name docs-drift --action spawn --cron '0 6 * * 1' \
  --spawn-profile <profile> --worker-deadline 2h --concurrency Forbid \
  --name-template 'docs-drift-{{fire_time}}' \
  --instruction 'Update this checkout to origin/main, then follow the docs-drift skill and file with -w <workspace>.'
```
