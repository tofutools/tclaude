---
name: process-templates
description: >-
  Create, inspect, validate, and safely CAS-edit tclaude ProcessTemplate YAML
  through `tclaude agent process-templates`. Use when a human asks an agent to
  design a process from scratch, change an existing process flow, add or revise
  nodes/performers/params/edges, preserve editor layout during conversational
  edits, or resolve a stale process-template save conflict. Covers the complete
  authoring shape, distinct process.templates.read/manage permissions, the
  show-edit-validate-save workflow, and execution-safety boundaries.
---

# Author process templates

Treat a process template as untrusted, declarative YAML. Authoring and saving
never starts a run, spawns an agent, or executes a program. Use only the
agentd-backed commands below; do not write directly into the process store.

## Permissions

A scribe needs both independent slugs:

- `process.templates.read` for `ls`, `show`, and `validate`. The optional
  default-permission installer grants this low-risk read slug.
- `process.templates.manage` for `save`. It is not default-granted. Ask the
  human to grant it, or use `save --ask-human 30s` for one-shot approval.

Manage does not imply read. Do not request execution, group-template, or other
permissions for authoring. A 403 names the missing slug; do not loop on it.

## Safe workflow

### Edit an existing template

Always start from the current head and preserve the complete document:

```bash
tclaude agent process-templates show release-flow > /tmp/release-flow.yaml
# Read the leading "# tclaude sourceHash: ..." comment, then edit the YAML.
tclaude agent process-templates validate --file /tmp/release-flow.yaml
tclaude agent process-templates save --file /tmp/release-flow.yaml \
  --expect-source-hash <hash-from-show>
tclaude agent process-templates show release-flow
```

`show` emits valid YAML. Its three leading comments carry `currentRef`,
`sourceHash`, and `semanticHash`; comments may remain in the edited file.
`validate` must succeed before `save`. After saving, re-show and verify.

CAS compares `--expect-source-hash` with the current head under the store lock.
On `process_template_conflict`, never retry with an empty or guessed hash and
never blind-overwrite. Re-show, merge the human's requested changes into the
new YAML while preserving unrelated fields/layout, validate, and save using
the new `sourceHash`. `semanticHash` identifies executable semantics;
`sourceHash` also changes for editor-only layout changes.

### Create a template

Write a complete YAML document, omit `layout`, then:

```bash
tclaude agent process-templates validate --file /tmp/new-process.yaml
tclaude agent process-templates save --file /tmp/new-process.yaml
tclaude agent process-templates show <new-id>
```

Omit `--expect-source-hash` only for creation. An empty expectation conflicts
if that id already exists, preventing an accidental overwrite.

## YAML shape

Use exactly:

- `apiVersion: tclaude.dev/v1alpha1`
- `kind: ProcessTemplate`
- a lowercase id matching `[a-z0-9][a-z0-9._-]*`
- top-level `start` naming an entry in `nodes`
- explicit `type` on every node
- inline `next` outcome-to-node mappings
- uniform `performer` blocks with a required `kind`

Top-level fields:

| Field | Purpose |
|---|---|
| `id` | Stable template key and route id. |
| `name`, `description`, `doc` | Optional human-facing prose. |
| `params` | Map of parameter ids to `{type, name?, description?, doc?, required?, default?}`. |
| `start` | Entry node id. A separate start-typed node is optional. |
| `nodes` | Map keyed by stable node id. |
| `layout` | Optional editor-owned node positions; follow the preservation rule below. |

Parameter references use `{{ params.issue }}` and must name a declared param.
They are active in performer `prompt`, `ask`, `run`, and `args`; validation
warns when placed in inert fields such as profile, timeout, or backoff.

### Node types

- `task`: require `performer`; usually route `pass` and `fail`. Optionally add
  `plan`, ordered `checks`, `review`, `retry`, and `captures`.
- `decision`: require a decider `performer`; each possible verdict needs an
  exact outcome key in `next`.
- `wait`: require `wait.duration`, `wait.until`, or `wait.signal`, plus `next`.
- `start`: optional explicit entry marker; require `next`.
- `end`: no `next`; optional `result` such as `success`, `failed`, or
  `canceled`.

Keep node ids stable during ordinary edits. Every non-end reachable node needs
a route that the runtime can take. Use bounded `retry.maxAttempts` for retry
loops and validate reachability, outcome vocabulary, and loop budgets.

### Performers

Every performer starts with `kind` and may use shared `profile`, `timeout`, and
`contact` (`cadence`, positive `budget`, `escalationTarget`). Kind-scoped fields:

- `kind: agent`: require `prompt`; optional `profile`, `model`, `effort`.
- `kind: human`: require `ask` or `prompt`; optional `profile`, `assignee`,
  `choices`. Decision choices route through exact `next` outcomes. Human task
  choices require `choiceOutcomes` mapping each label to `pass` or `fail`.
- `kind: program`: require `run`; optional literal `args` and `timeout`.
  Saving still executes nothing. A later run needs its separate explicit
  program-execution opt-in and security review.

For compound task stages, each `plan`, `checks[]`, and `review` entry requires
an `id` and a `performer`. Only `plan` accepts `approval: human|auto` and
`approvalRetry`. Put retry policy on the stage it budgets.

### Generation example

```yaml
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: code-change
name: Code change
description: Implement, test, review, and request merge approval.
params:
  issue:
    type: string
    required: true
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: Implement {{ params.issue }}
    checks:
      - id: tests
        performer:
          kind: program
          run: go
          args: [test, ./...]
        retry:
          maxAttempts: 2
      - id: cold-review
        performer:
          kind: agent
          profile: reviewer
          prompt: Cold-review the diff for {{ params.issue }}
    review:
      id: merge-approval
      performer:
        kind: human
        profile: operator
        ask: Approve merge?
    retry:
      maxAttempts: 3
      onFail: feedback-same-session
    next:
      pass: done
      fail: escalate
  escalate:
    type: decision
    performer:
      kind: human
      profile: operator
      ask: Retries exhausted. Continue?
    next:
      retry: implement
      cancel: canceled
  done:
    type: end
    result: success
  canceled:
    type: end
    result: canceled
```

Start with the smallest graph that expresses the request. Add compound stages,
waits, contact schedules, programs, or retries only when the process needs
them. Do not invent execution side effects while authoring.

## Preserve editor-owned layout

`layout` is presentation metadata attached to `(id, semanticHash)` and excluded
from semantic identity.

- For a new template, omit `layout`; the editor derives it.
- For an edit, preserve the entire existing `layout` for every surviving node
  id. Never recompute, normalize, or casually delete positions.
- Remove positions only for nodes actually removed. New nodes may omit
  positions so the editor places them.
- A layout-only edit changes `sourceHash` without changing `semanticHash` or
  `currentRef`; it still requires CAS.

Agent saves append actor/source-hash attribution to the version record. This
retains successive layout-only authoring events that share one semantic ref.

## Validation and safety

Treat every error diagnostic as blocking and fix it before save. Review
warnings deliberately; do not suppress them by deleting unrelated content.
Unknown fields, duplicate keys, bad ids, unreachable nodes, missing targets,
unbounded retry loops, undeclared params, wrong-kind performer fields, and
invalid durations all surface through validation.

Saving only persists canonical authoring content, selects a head with CAS, and
records the socket peer's stable actor identity. It must never be used as an
execution mechanism. Use `ls` and `show` to inspect the result through the same
store and REST surface the dashboard consumes.
