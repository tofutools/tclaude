# Architecture

The daemon owns durable work. CLI and browser clients ask the application to
perform operations; neither client edits storage or chooses its authenticated
identity in a request body.

## Common concepts

| Concept | Meaning |
|---|---|
| Agent | Stable worker identity, desired configuration, lineage and mailbox |
| Conversation | Logical history independent of a particular running process |
| Execution | One concrete attempt to run a harness or standalone shell |
| Workspace | An owned checkout with separate resource and use lifetimes |
| Work run | Pinned assignment or graph, exact attempts, evidence and decisions |
| Definition | Immutable revision of a reusable process, team or profile |
| Operation | Admitted effect with attribution, authority and durable settlement |
| Decision | Revision-checked answer to an exact access request or work question |

A provider's native session ID is private evidence about a conversation. It is
not the identity of an agent, execution or work run. An inherited action
credential represents its execution; it does not prove native primary-session
provenance. Provider-owned observation ingress establishes that separate fact
within its disclosed native trust boundary.

## Code ownership

`internal/backend/model` defines durable concepts. `app` owns workflows,
authority admission and settlement. `sqlite` implements their transactional
storage. `ports` defines the focused host/provider contracts those workflows
need. Cohesive implementations under `providers` own native formats, launch and
history semantics. `host` owns processes, terminal resources and Git checkouts.

`transport` projects public DTOs and authenticates requests. `server` composes
those layers and owns background-worker cancellation and join. `internal/product`
contains the shared CLI commands and local browser. The two root binaries only
establish their process lifetime and execute those commands.

Providers expose capabilities such as history, attachment and native guidance
when supported. The application pins the selected configuration and history
revision, admits current authority immediately before an effect, and settles the
result independently of a disconnected caller. Recovery uses exact retained
resource evidence. Unknown effects stay uncertain until observed or explicitly
resolved; restarting does not replay them.

## Why these boundaries exist

The earlier backend grew around session management. A session row, tmux pane,
native conversation, agent and running task accumulated overlapping lifecycle
responsibilities. Harness differences and coordination policy often appeared in
the same command or daemon path.

The current model separates those lifetimes and owners. A cancelled work outcome
can still own a running execution and checkout. A retired agent keeps historical
attribution. A configuration update describes the next launch without silently
changing a current process. A native reset changes context only when correlated
evidence confirms it. These distinctions are part of the application contract,
shared by every client and harness.

Offline migration reads a frozen versioned source schema and translates important
durable meaning into these types. Old runtime handles and ambiguous authority do
not become active. Original inactive authoring and conversion diagnostics remain
explicitly inspectable.
