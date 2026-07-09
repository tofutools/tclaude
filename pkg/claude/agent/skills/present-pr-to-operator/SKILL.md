---
name: present-pr-to-operator
description: >-
  Present a pull request intentionally in the tclaude operator dashboard via
  `tclaude agent present-pr <url>`. Use when you have opened or updated a PR
  and want the human/operator to see it even if branch/statusline PR detection
  has not picked it up. Requires `self.pr` for your own PR (default-granted by
  `tclaude setup --install-default-agent-permissions`); manager pattern:
  `tclaude agent present-pr <url> --target <peer>` requires `agent.pr`, OR
  ownership of a group containing the target.
---

# Present PR To Operator

Use this when your PR is ready for the human/operator to notice in the
tclaude dashboard:

```bash
tclaude agent present-pr https://github.com/owner/repo/pull/42 --summary "ready for review" --state open
```

The URL must be http(s). GitHub PR URLs get a `#42` badge automatically;
`--summary` is a short optional tooltip/label. The dashboard also keeps its
existing branch/statusline PR detection and dedupes by PR URL, so presenting a
PR is safe even when the automatic path already found the same link.

When the PR no longer needs operator attention:

```bash
tclaude agent present-pr https://github.com/owner/repo/pull/42 --handled
```

If you see a permission error for your own PR, ask the human to grant:

```bash
tclaude agent permissions grant default self.pr
```

Leads can present or handle a worker's PR with:

```bash
tclaude agent present-pr https://github.com/owner/repo/pull/42 --target worker-name
```

That cross-agent form requires `agent.pr`, unless the caller owns a group that
contains the target.
