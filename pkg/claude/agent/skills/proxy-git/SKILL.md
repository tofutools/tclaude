---
name: proxy-git
description: >-
  Fetch, push, and open GitHub pull requests through `tclaude proxy git` and
  `tclaude proxy github` when your own sandbox has no credentials — the
  `tclaude agentd` daemon runs git and gh on the host with ITS SSH key and
  GitHub token, so you never hold them. Use when a plain `git push`, `git
  fetch`, or `gh pr create` fails with a permission, authentication, or network
  error, or when you have been told the daemon holds the credentials. Gated on
  the `git.read` / `git.push` / `github.read` / `github.write` slugs, none of
  which is granted by default, and bounded by an operator allow-list of remotes.
---

# Git and GitHub without holding the credential

If your sandbox denies `~/.ssh` or `~/.config/gh`, or denies the network, then
plain `git push` and `gh pr create` cannot work — and that is deliberate. Route
them through the daemon instead:

```bash
tclaude proxy git push -u
tclaude proxy github pr create --title "Add the thing" --body-file pr.md
```

`tclaude agentd` runs on the host, where the credentials live. You describe the
operation; it builds the command. You never see a key or a token, and there is
no way to pass it a command line of your own.

## Start with `git remotes`

Before anything else, run:

```bash
tclaude proxy git remotes
```

It needs no network and no credential, and it tells you three things you would
otherwise have to discover from a 403: which remotes your repository has,
whether each one is on the operator's allow-list (with the reason when it is
not), and which branches you may not push to.

If a remote is refused, that output is what you quote to your human — it names
exactly what they need to add to `agent.git_proxy.allowed_remotes`.

## The commands

```bash
# Reads — need `git.read`
tclaude proxy git remotes                       # allow-list verdict per remote
tclaude proxy git ls-remote --heads             # does my branch exist remotely?
tclaude proxy git fetch --prune
tclaude proxy git pull                          # fetch, then fast-forward locally

# Writes — need `git.push`
tclaude proxy git push -u                       # push the current branch
tclaude proxy git push -b feat/thing
tclaude proxy git push --force-with-lease       # only if the operator enabled it

# GitHub reads — need `github.read`
tclaude proxy github pr ls --state open
tclaude proxy github pr view 42
tclaude proxy github pr checks 42               # CI state; pending is an answer
tclaude proxy github issue ls
tclaude proxy github issue view 7

# GitHub writes — need `github.write`
tclaude proxy github pr create --title "…" --body-file pr.md [--draft] [--base main]
tclaude proxy github pr comment 42 --body-file reply.md
tclaude proxy github pr ready 42
tclaude proxy github issue comment 7 --body-file note.md
```

Use `--body-file` (or `--body-file -` for stdin) for anything multi-line. It
sidesteps shell quoting — backticks especially — and keeps the text out of the
process command line.

## What you cannot do, and why

**You cannot choose the repository.** There is no `--repo` flag. The daemon uses
the git work tree containing the directory you were launched in, and derives the
GitHub repo from that repository's remote. This is the rule that makes the whole
feature safe to grant: the proxy lends you *credentials*, never *reach*.

**You cannot push to a protected branch.** `main` and `master` (and whatever
else the operator listed) are refused outright. Push a feature branch and open a
pull request — which is the workflow anyway.

**Force-pushing is off unless the operator turned it on**, and plain `--force`
does not exist here at all; only `--force-with-lease`, which refuses to discard
commits you have not seen.

**`pull` only fast-forwards.** The daemon fetches; the merge runs locally as
you. If your branch has diverged, you get told so and you resolve it yourself
(rebase or merge) — the daemon will not merge for you.

**Everything you create on GitHub is attributed to the operator's account.** A
PR you open is a PR they opened. Write accordingly.

## Reading the result

These commands report git's and gh's own verdict rather than swallowing it. A
non-zero exit with `! [rejected] … (non-fast-forward)` means exactly what it
means with plain git: fetch and rebase, then push again.

A `403` naming a slug means you lack the permission. Ask your human to grant it:

```bash
tclaude agent permissions grant <you> git.push
```

Or, for a single operation, request one-off approval:

```bash
tclaude proxy git push --ask-human 60s
```

That raises an approval popup for the human. Timeout counts as denial — if you
are denied, say so and stop; do not retry in a loop.

A `503` saying the proxy is not enabled means the operator has not configured
`agent.git_proxy.allowed_remotes` at all. That is a conversation to have with
them, not something to retry.
