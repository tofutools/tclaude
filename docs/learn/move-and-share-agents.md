# Move and share agents

An agent's value is its context: the task, what it tried, and what you
decided together. In this tutorial you save that context to a file in the
Portable Agent Format (PAF), read it without any harness, land it on another
harness or a smaller model, fork it from a snapshot, and move an agent to
another machine.

**You need:** [Your first agent team](first-team.md) completed, with the
daemon running and `TCLAUDE_HUMAN_TOKEN` exported. Step 6 also needs two
linked machines; the other steps work on one.

## 1. Give an agent something worth saving

Create a group and an agent that does a bit of work:

```bash
cd ~/code/my-project
tclaude agent groups create lab --descr "PAF tutorial"
tclaude agent spawn lab --name scout \
  --initial-message "Read this project and write a short list of its main packages and what each does."
```

Wait until `scout` is idle in the dashboard (or in `tclaude session watch`).

## 2. Export it to a file

```bash
tclaude paf export scout --history --file scout.paf
```

`--history` includes the conversation. It travels twice: as the harness's own
native history, for a lossless resume on the same harness, and as a
harness-neutral copy that any harness can land. The export refuses if the
conversation contains text that looks like a credential.

!!! info "Go deeper"
    [PAF → Exporting and importing an agent](../paf.md#exporting-and-importing-an-agent)
    covers every export option.

## 3. Read it without any harness

A PAF can always be read, even with no harness, model or network:

```bash
tclaude paf inspect scout.paf
tclaude paf render scout.paf --out scout.html
```

`inspect` shows what the file holds: the harness and model the agent ran on,
its working directory, signatures, and any privacy findings. It does not
show the conversation. `render` writes the whole conversation as one
self-contained HTML page (or Markdown, without `--out`). Open `scout.html` in
a browser.

!!! info "Go deeper"
    [PAF → Reading a PAF](../paf.md#reading-a-paf).

## 4. Land it on another harness or a smaller model

Importing creates a **new** agent from the file. It gets a new identity; the
file records who the agent was, never what it may do here. Import previews
first and changes nothing until you add `--apply`.

Preview a landing on another harness:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --harness codex
```

Look at `history_landing` in the preview. On the same harness the
conversation lands **native**, with nothing lost. On another harness it lands
**translated**: the preview lists what does not carry over, such as the old
model's reasoning.

Landing on a model with a smaller context window works the same way:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --model sonnet
```

When the conversation does not fit the new window, the preview says how much
would be cut. From a 25% cut, `--apply` refuses until you choose: accept the
cut with `--confirm-history-loss`, pick a model with a larger window, or
import without history using `--skip-history`. A short tutorial conversation
fits easily, so you will most likely see no cut at all.

Land it for real:

```bash
tclaude paf import --file scout.paf --cwd "$PWD" --harness codex --group lab --name scout-codex --apply
```

The new agent, `scout-codex`, joins `lab` and continues from the imported
conversation. `--name` keeps it apart from the original `scout`. The
full history is never lost: it stays in the file, and when a landing cuts
it, a copy is saved beside the new agent.

!!! info "Go deeper"
    [PAF → Landing on a smaller context window](../paf.md#landing-on-a-smaller-context-window)
    explains the budget and `--with-summary`, which asks the agent to write a
    summary first so a cut keeps more.

## 5. Fork agents from a snapshot

A snapshot is a named save point in this machine's library. The agent carries
on; the snapshot stays as it was.

```bash
tclaude agent snapshot create scout --name scout-v1 --note "after the package survey"
tclaude agent snapshot ls
```

Start two independent agents from it, each with its own identity and a copy
of the conversation:

```bash
tclaude agent spawn lab --from-snapshot scout-v1 --count 2 --name trial
```

They appear as `trial-1` and `trial-2`. `scout` itself is untouched. This is
how you try two approaches from the same starting point.

!!! info "Go deeper"
    [PAF → Snapshots](../paf.md#snapshots) and
    [Spawning from a snapshot](../paf.md#spawning-from-a-snapshot).

## 6. Move an agent to another machine

This step needs two tclaude instances linked through a hub. If you have not
set that up, [Federation setup](../federation-setup.md) walks through it;
skip this step until then. Below, the other machine is the trusted peer
`laptop`, and it has a group called `helpers`.

Move `scout`, as the operator:

```bash
tclaude federation move-agent scout laptop --group helpers
```

On `laptop`, list the offer, preview it, and accept it:

```bash
tclaude federation offers
tclaude federation offers import OFFER_ID --cwd ~/code/my-project
tclaude federation offers import OFFER_ID --cwd ~/code/my-project --apply
```

Once the agent runs on `laptop`, the source retires `scout`. Until then the
source keeps running, and `tclaude federation moves ls` shows the progress.

An agent can also move itself with `tclaude agent teleport`. It needs a
grant scoped to the destination:

```bash
tclaude agent permissions grant scout self.teleport --scope peer=laptop
```

Then ask `scout` to run `tclaude agent teleport laptop --group helpers`.

!!! info "Go deeper"
    [Federation → Moving an agent](../federation.md#moving-an-agent) and
    [Teleport yourself to another node](../federation.md#teleport-yourself-to-another-node).

!!! note "Sharing snapshots on a Board"
    You can also share a snapshot, encrypted to named members, on a federation
    Board with `tclaude agent snapshot share`. This needs a hub recent enough
    to support board snapshots: update `tclaude-hub` first, because an older
    hub is refused before anything is uploaded. See
    [PAF → Sharing a snapshot on a Board](../paf.md#sharing-a-snapshot-on-a-board).

## 7. Clean up

```bash
tclaude agent groups retire lab
tclaude agent groups archive lab
tclaude agent snapshot rm scout-v1
```

## What you learned

- `tclaude paf export --history` saves an agent, conversation included, to
  one file.
- `paf inspect` and `paf render` read any PAF with no harness or model.
- `paf import` previews, then lands a new agent on any harness or model, and
  asks before cutting history to fit.
- Snapshots are save points; `agent spawn --from-snapshot` forks new agents
  from them.
- `federation move-agent` and `agent teleport` move an agent between
  machines.

The full format, sharing, redaction and resilience snapshots are in the
[Portable Agent Format](../paf.md) reference.
