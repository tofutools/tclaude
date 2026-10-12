# Keep agents going

An agent's context window fills up as it works. Eventually it has to start
fresh, or move to a model that suits the work better. In this tutorial you
watch an agent's context, and use the dashboard to clone it, reincarnate it,
and change its model in place.

**You need:** [Find and resume your work](find-and-resume.md) completed,
with `alice` online in `hello`.

## 1. Watch the context

Each row's **State** column has a small meter beside the pill: how full the
agent's context window is. Hover it for the numbers. When it gets high, the
harness compacts the conversation on its own, summarizing it and carrying on,
or you can step in with the actions below.

!!! info "Go deeper"
    [Dashboard → The context meter](../dashboard.md#the-context-meter).

## 2. Clone or reincarnate

Two actions in an agent's **⚙** menu make a new session from an old one:

- **Clone** forks the agent. The original keeps running, and a sibling,
  `alice-c-1`, starts with a copy of its conversation, groups and
  permissions. Use it to try a second approach in parallel.
- **Reincarnate** replaces the agent. A fresh session with an empty context
  window takes over its identity: agent id, groups, permissions and inbox.
  Use it when the context is full of things the agent no longer needs.

Open `alice`'s **⚙** menu and choose **clone…** under **send elsewhere**.
Pick your own node, marked **(this node)**, and click **Clone here…**. The
clone dialog opens.
Leave **Copy conversation history** ticked, give it a **Follow-up** such as
`List the project's dependencies.`, and click **Clone**. `alice-c-1` appears
next to `alice`. Retire it with its bin button
when you are done.

Now choose **reincarnate** in `alice`'s **⚙** menu. Keep the recommended
**Ask the agent to reincarnate itself**: `alice` writes a handoff for her
successor at her next clean point, then reincarnates. An optional **Focus hint**
tells her what the handoff should cover. Click **Ask agent**. (**Force
reincarnate now** is for an agent that is stuck: it skips the handoff.)

The replaced session is kept: **▾ view** → **show replaced generations**
lists it.

## 3. Change model in place

**Morph** relaunches an agent with a different model, effort or harness. It
keeps the agent's conversation, id, inbox, groups and permissions. The
dashboard morphs into a **spawn profile**, a saved set of launch settings,
so first save one:

1. Open the spawn dialog on `hello`, pick another **Model** (for example
   Sonnet), and click **Save as profile…**. The profile editor opens:
   name it `sonnet` and save it, then cancel the spawn dialog.
2. Open `alice`'s **⚙** menu → **morph…**, and pick `sonnet` under
   **Profile**.

The dialog previews the change, **was** and **now**, before anything
happens. A profile for another harness moves `alice` there: tclaude
translates her conversation, and the preview says what the history keeps
and loses.

Click **Morph at next idle turn**. If `alice` is busy, the morph waits until
she is idle (within 30 minutes, or the request lapses). Tick **now** to stop
her at once instead.

!!! info "Go deeper"
    [Spawning and lifecycle → morph](../spawning-and-lifecycle.md#morph)
    covers everything a morph changes and keeps, and
    [Harnesses → Cross-harness history translation](../harnesses.md#cross-harness-history-translation)
    what a harness move carries.

## From the terminal

The CLI has a few things the dashboard does not: compacting on demand,
morphing without a profile, and undoing a morph.

```bash
tclaude agent context-info -g hello           # context of every member
tclaude agent compact -t alice "Continue where you left off."
tclaude agent clone -t alice "List the project's dependencies."
tclaude agent reincarnate -t alice "Summarize how this project is structured."
tclaude agent morph alice --model sonnet --dry-run
tclaude agent morph alice --back              # undo the last morph
```

[Lifecycle verbs](../spawning-and-lifecycle.md#lifecycle-verbs) covers each
of them.

## What you learned

- The context meter shows how full an agent's window is.
- **Clone** forks a sibling with a copy of the conversation; **reincarnate**
  replaces the agent with a fresh session that keeps its identity.
- **Morph** changes model, effort or harness in place through a spawn
  profile, with a preview first.

**Next:** you have finished the path. The [Dashboard](../dashboard.md)
reference covers every other tab, including **Usage** and **Costs** for your
quota and spend.
