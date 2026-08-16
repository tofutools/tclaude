---
name: present-pr-to-operator
description: >-
  Present a pull request intentionally in the tclaude operator dashboard via
  `tclaude agent present-pr <url>`. Use when you have opened or updated a PR
  and want the human/operator to see it even if branch/statusline PR detection
  has not picked it up. Requires `self.pr` for your own PR (default-granted by
  `tclaude setup --install-default-agent-permissions`); manager pattern:
  `tclaude agent present-pr <url> --target <peer>` requires global `agent.pr`,
  or `groups.members.pr` covering every current active group containing the
  target; ownership contributes the latter for owned groups.
---

# Present PR To Operator

Use this when your PR is ready for the human/operator to notice in the
tclaude dashboard:

```bash
tclaude agent present-pr https://github.com/owner/repo/pull/42 --summary "ready for review" --state open
```

The URL must be a canonical `https://github.com/<owner>/<repo>/pull/<number>`
URL. Run the command from that repository: the daemon verifies that its origin
matches the PR, that the repository is inside your recorded launch directory,
and that the operator allow-listed it in `agent.git_proxy.allowed_remotes`.
`--summary` is a short optional tooltip/label. The dashboard also keeps its
existing branch/statusline PR detection and dedupes by PR URL, so presenting a
PR is safe even when the automatic path already found the same link.

For GitHub PR URLs, the agent daemon refreshes the PR state on the dashboard's
normal polling cadence. Merged or closed PRs remain visible briefly, then are
omitted automatically.

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

That cross-agent form requires global `agent.pr`, or `groups.members.pr`
covering every current active group containing the target.
