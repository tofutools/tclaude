# Cross-machine (far future)

**Explicitly out of scope for now** — single-host first.

When/if we ever want to span hosts: federate `tclaude agentd`
instances over the network. Each host's daemon owns its local conv
pool and proxies messages destined for remote convs to the
appropriate peer daemon. Keeps the per-host peer-cred identity model
intact.

## Legacy git-sync sketch (pre-agentd era)

For now everything is keyed off the local SQLite + filesystem inbox.
A future variant could publish messages over the existing `git` sync
channel (`pkg/claude/git`) so agents on different machines can talk.

Likely needs:
- A real message-id namespace (UUIDs) and conflict-free message
  ordering.

This is one of several possible directions; federated HTTP-over-Unix-
socket between daemons is probably more idiomatic now that agentd
exists.

## Files
- `pkg/claude/agentd/` — would gain federation handlers
- `pkg/claude/git/` — alternate transport sketch
