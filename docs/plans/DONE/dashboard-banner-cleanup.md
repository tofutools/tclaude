# Dashboard banner cleanup (2026-05)

The dashboard's "Inline edits available for member ownership and
removal…" banner at the top of every page has been removed. It
duplicated UX hints already next to the relevant controls (hover
affordances on member rows, per-row "Copy CLI command" buttons) and
just cost vertical space on every load.

## What was removed

In `pkg/claude/agentd/dashboard.html`:

- The `<div class="banner">…</div>` element (was at the top of
  `<body>`, between the header and the tab nav).
- The `.banner` CSS rule.
- The `#refresh-link` click handler that the banner's "refresh
  now" anchor relied on. The element no longer exists, so the
  handler had nothing to bind to. Auto-refresh (5s `setInterval`)
  continues to work — humans can also press F5 / their browser's
  refresh.

No JS or backend behaviour changed. Pure visual cleanup.

## Future startup tips

If we later decide we *do* want startup hints, the original TODO
sketched the proper shape: dismissable per-tip, contextual, rotating
pool, killable globally. Don't ship a static banner again — build
proper tips with persistence on a concrete UX gap. Until then: no
tips.

## Files

- `pkg/claude/agentd/dashboard.html`
